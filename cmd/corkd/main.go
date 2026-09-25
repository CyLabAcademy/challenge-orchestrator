package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/CyLabAcademy/challenge-orchestrator/cork"
)

type state struct {
	mgr *cork.Manager
}

// retryableLaunch is what a launch can fail with where the caller should
// simply place it again. Every worker being skipped is retryable when they
// are states a worker leaves on its own -- overloaded, or unresponsive, which
// the probes recover from without anyone intervening. Every worker having
// been taken down by an operator is not going to resolve itself, so it stays
// a 500 (ErrAllWorkersDown is deliberately absent here). A launch refused by
// its own worker -- no slot in time, a docker call timed out on a slow one,
// the worker stopped answering under it, or its image pull timed out -- is
// retryable too, as is one that lost the race for the database's write lock:
// the retry is placed afresh.
var retryableLaunch = []error{
	cork.ErrAllWorkersOverloaded,
	cork.ErrAllWorkersUnresponsive,
	cork.ErrWorkerBusy,
	cork.ErrWorkerUnreachable,
	cork.ErrPullTimeout,
	cork.ErrDatabaseBusy,
}

// retryableStop is the same for a stop, which places nothing and so can only
// lose the race for the database's write lock, retryable like a launch that
// did.
var retryableStop = []error{cork.ErrDatabaseBusy}

// errorResponse maps a failed manager call onto the response code, and onto
// whether the caller is being told to try again -- which the handler turns
// into a Retry-After. An unknown identifier is a 404; anything in retryable
// is a 503; everything else is a 500.
//
// The unknown-identifier test is a type assertion rather than errors.As, so
// a wrapped one reports 500. That is the behavior the handlers have always
// had, and the manager returns these bare.
func errorResponse(err error, retryable []error) (code int, retry bool) {
	if _, ok := err.(*cork.UnknownIdentifierError); ok {
		return http.StatusNotFound, false
	}
	for _, target := range retryable {
		if errors.Is(err, target) {
			return http.StatusServiceUnavailable, true
		}
	}
	return http.StatusInternalServerError, false
}

func main() {
	var iface string
	var port int
	var help bool
	var version bool
	flag.IntVar(&port, "port", 4200, "listening port for corkd")
	flag.StringVar(&iface, "address", "", "listening address for corkd")
	flag.BoolVar(&help, "help", false, "display usage information")
	flag.BoolVar(&version, "version", false, "display version information")
	flag.Parse()

	if version {
		fmt.Printf("Version: %s\n", cork.Version())
		os.Exit(0)
	}

	if help {
		printUsage()
		os.Exit(0)
	}

	// CORK_LOGGING, which this daemon documented and did not read: it is
	// the only way to raise an orchestrator's logging, since there is no
	// flag for it. A level that does not parse is complained about and not
	// fatal -- a typo here is no reason to refuse to serve.
	logLevel, logLevelErr := cork.LogLevelFromEnv(cork.INFO)
	if logLevelErr != nil {
		// Before the manager, not after: a start that then fails would
		// otherwise never say why its logging was not what the unit file
		// asked for, and this is the one complaint that explains the rest
		// of the output.
		log.Printf("warning: %s", logLevelErr)
	}
	mgr := cork.NewManager(logLevel)
	if mgr == nil {
		log.Fatal("failed to initialize cork library")
	}

	// corkd is the sole owner of the database and the docker/registry state:
	// every action (deploys, builds, instance lifecycle, worker management)
	// goes through its HTTP API. New instances are placed on the configured
	// workers (round robin, skipping overloaded and unreachable ones).
	mgr.EnableWorkerPlacement()

	s := state{mgr: mgr}

	http.HandleFunc("/challenges", s.listHandler)
	http.HandleFunc("/challenges/", s.challengeHandler)
	http.HandleFunc("/builds/", s.buildHandler)
	http.HandleFunc("/instances/", s.instanceHandler)
	http.HandleFunc("/workers", s.workersHandler)
	http.HandleFunc("/workers/", s.workerHandler)
	http.HandleFunc("/pins", s.pinsHandler)
	http.HandleFunc("/schemas", s.schemaHandler)
	http.HandleFunc("/schemas/", s.existingSchemaHandler)
	http.HandleFunc("/update", s.updateHandler)
	http.HandleFunc("/state", s.stateHandler)
	http.HandleFunc("/version", s.versionHandler)

	connStr := fmt.Sprintf("%s:%d", iface, port)
	log.Fatal(http.ListenAndServe(connStr, nil))
}

func printUsage() {
	fmt.Printf(`
Usage: %s [<options>]
  --address  the network address to listen on (default: 0.0.0.0)
  --port     the port to listen on (default: 4200)
  --help     display this message
  --version  display version information and exit

Relevant environment variables:
  Each of these also answers to the CMGR_ name it had before the rename
  (CMGR_DB for CORK_DB, and so on), read only when the CORK_ name is unset.
  The daemon lists the CMGR_ names it finds at startup; they go away two
  minor releases after the rename.

  CORK_DB - path to cork's database file (defaults to 'cork.db'; a run
      before the rename that set nothing made 'cmgr.db' instead, and
      nothing moves it, so point CORK_DB at it or start afresh)

  CORK_DIR - directory containing all challenges (defaults to '.')

  CORK_ARTIFACT_DIR - directory a build's artifact bundle is written to
      (defaults to '.'). Local build plane only: cork does not serve
      artifacts and a hand-over carries none, so on CORK_BUILD_PLANE=external
      this is read by nothing and saying so is all it gets.

  CORK_MAX_ARTIFACT_FILES - maximum number of entries permitted in a
      challenge's artifact archive (defaults to 10000); local build plane
      only, as the two below are

  CORK_MAX_ARTIFACT_BYTES - maximum total uncompressed size of a challenge's
      artifact archive (defaults to '5g')

  CORK_MAX_ARTIFACT_FILE_BYTES - maximum uncompressed size of any single file
      within a challenge's artifact archive (defaults to '1g')

  CORK_LOGGING - controls the verbosity of the internal logging infrastructure
      and should be one of the following: debug, info, warn, error, or disabled
      (defaults to 'info')

  CORK_PORTS - the range of ports that are dedicated for serving challenges;
      cork will assume that it fully owns these ports and nothing else will
      try to use them (i.e., not in ephemeral range or overlapping with a
      service running on the host); format is '1000-1000'

  CORK_INTERFACE - the host interface/address to which published challenge
      ports should be bound (defaults to '0.0.0.0'); if the specified interface
      does not exist on the host running the Docker daemon, Docker will silently
      ignore this value and instead bind to the loopback address

  CORK_PRUNE_AGE - the maximum age for on-demand challenge instances; old
      instances are automatically pruned from the database (defaults to '1h');
      set to '0' to disable automatic pruning.
  CORK_DB_WAL - controls whether SQLite WAL journaling mode is enabled;
      on by default for improved throughput under high concurrency;
      creates <db>-wal and <db>-shm sidecar files; do NOT use on network-mounted
      filesystems (NFS, SMB) as this may cause corruption; set to 'false'
      to disable.

  CORK_CONCURRENT_LAUNCHES - launch slots per docker daemon (defaults to 2;
      1 to 16). A slot covers an instance's network creation and container
      starts, which dockerd serializes internally on either firewall
      backend: 2 measured as the optimum on iptables, and nftables showed no
      gain past 2 either (it makes each launch faster, not more parallel).
      The range is open for re-measuring, not for tuning. As many teardown
      slots bound the stops in flight on a daemon: a deluge of stops queues
      in corkd instead of inside dockerd, which it used to make slow, then
      unresponsive.

  Each worker carries two states, from two agents on two ports. 'reachable'
  (ok / unresponsive / down) comes from its docker daemon and says whether a
  launch there could work at all. 'load' (ok / overloaded / unknown) comes
  from its telemetry agent and says whether one should be added. A worker
  takes placements when it is reachable and not overloaded -- an unknown load
  still places, since a box whose telemetry agent died is usually serving
  perfectly well and holding it out of a small fleet costs more.

  Only 'down' is asserted rather than observed: an operator sets it with
  worker-down, and no probe will lift it. 'unresponsive' is cork's own
  verdict, and the worker recovers from it on its own once its daemon answers
  again -- reconnecting and reconciling first, exactly as a worker-add does.

  CORK_WORKER_POLL_INTERVAL, CORK_WORKER_POLL_TIMEOUT, CORK_WORKER_PING_TIMEOUT -
      how often each worker is probed (defaults to '5s'), the telemetry
      timeout (defaults to '250ms') and the docker /_ping timeout (defaults
      to '1s'); each is clamped to half the interval when not under it.

  CORK_WORKER_DEEP_PROBE_INTERVAL - how often the deeper docker probe runs, a
      one-container list (defaults to '30s'). A daemon can answer /_ping while
      wedged underneath, which the ping alone would never notice; the listing
      is capped at one container so its cost does not grow with the fleet's.

  CORK_WORKER_MAX_MISSES, CORK_WORKER_LOAD_MISSES - consecutive failed docker
      probes before the worker is ejected as unresponsive (defaults to 6, i.e.
      30s), and consecutive failed telemetry polls before its load is unknown
      (defaults to 3). Tighter for load on purpose: unknown still places, so
      being wrong about it costs a flag rather than a worker.

  CORK_WORKER_HEALTHY_THRESHOLD, CORK_WORKER_RECOVER_BACKOFF,
  CORK_WORKER_RECOVER_BACKOFF_MAX, CORK_WORKER_EJECTION_DECAY - how a worker
      comes back: consecutive good probes required (defaults to 2, so a daemon
      flapping as it starts does not bounce in and out of placement), the wait
      before a recovery attempt multiplied by how many times the worker has
      been ejected (defaults to '10s', capped at '5m'), and how long it must
      then run clean to have one ejection forgiven (defaults to '5m').

  CORK_WORKER_CONTROL_TIMEOUT - ceiling for one container or network call to
      a worker's docker daemon (defaults to '30s'). A call that fails at the
      connection level ejects the worker at once. One that hits the timeout
      has cork ping the daemon: no answer ejects it at once too, but a daemon
      that answers is slow rather than gone -- dockerd blocks every create
      while it deletes an evicted image -- and stays in placement.

  CORK_WORKER_TIMEOUTS_TO_EJECT, CORK_WORKER_TIMEOUT_WINDOW - how many
      separate stalls, against a daemon that still answers pings, eject the
      worker, and within what window (defaults to 3 within '2m'). One stall
      counts once however many calls it times out; stalls are a control
      timeout apart at least. That pattern is a daemon wedged underneath a
      /_ping that answers.

  CORK_WORKER_PULL_TIMEOUT - ceiling for one image pull before a launch
      (defaults to '30s'); a pull that hits it fails that launch as
      retryable (503) but does not eject the worker, the registry being the
      likelier culprit. It is also the ceiling for a restart's pull whenever
      it is set above the five minutes those get by default.

  CORK_WORKER_LAUNCH_WAIT - how long a launch waits for a launch slot on its
      daemon (defaults to '10s'); past that it fails as retryable (503 with
      Retry-After) instead of queueing behind a saturated daemon. A launch
      that would evidently wait longer, judging by the launches already
      waiting there and the daemon's recent pace, is refused the same way at
      once, before anything is recorded.

  CORK_EMF_ENDPOINT - 'host:port' of a local CloudWatch agent listening for
      embedded metric format (the agent's default is '127.0.0.1:25888', and
      it listens once its config carries logs.metrics_collected.emf). When
      set, corkd sends one datagram per launch carrying that launch's total
      and its image, slot, network and container stages, plus the state of
      the daemon it ran against -- how many launches were queued there, how
      many slots were busy, whether the image had to be pulled. Each is
      attributed to the worker's public address, falling back to the private
      one when it has none; the private address rides along as a field, so
      the machine behind a name is still in the record without paying for a
      metric per machine. Unset, the
      default, exports nothing; the same figures are still summarized per
      worker in worker-list either way.

      The export cannot delay a launch: the launch records its sample to a
      bounded queue and an emitter goroutine owns the socket, so a slow,
      wedged or absent agent is never waited on. A full queue drops samples
      rather than blocking, and says so in the log once a minute.

      An agent that is not answering is retried every 30s and complained
      about every five minutes until it does, so a log read after a
      missing-data alarm shows whether the export is still broken rather
      than only that it once was. Recovery needs two writes in a row to
      succeed, since a connected UDP socket reports the refusal of the
      previous write; it then says the endpoint is accepting samples again.
      Both are driven by launches arriving rather than a timer, so an idle
      daemon stays quiet -- nothing is being lost there to report.

  CORK_EMF_LOG_GROUP - the CloudWatch log group those records are filed
      under (defaults to 'cork'). The agent takes it from each record, so
      nothing in the agent's own config names it.

  CORK_EMF_NAMESPACE - the CloudWatch namespace of the metrics extracted
      from those records (defaults to 'cork').

  CORK_REGISTRY - the docker registry holding built challenge images; when
      set, images are pulled from it before each instance start (must match
      the value used by cork when building).

  CORK_BASE_PINS - path to a JSON map of base image reference to digest,
      defaulting to <CORK_DIR>/.base-pins.json; absent or empty disables
      pinning. When set, corkd rewrites a FROM name:tag instruction to the
      equivalent digest reference as it builds each build context, so the
      builder never re-resolves a mutable tag against the registry and a base
      only moves when the pins are refreshed (POST /pins). Challenge
      Dockerfiles are never modified on disk.

  CORK_BUILD_PLANE - where challenge images are built: 'local' (the default)
      on the docker daemon DOCKER_HOST names, from the tree in CORK_DIR; or
      'external', where something else builds, pushes to CORK_REGISTRY and
      hands the finished builds to corkd (PUT /challenges/<id>, under HTTP
      API below). External means no local docker
      daemon and no challenge tree at all: CORK_DIR, CORK_BASE_PINS,
      DOCKER_HOST and CORK_PURGE_AFTER_PUSH are ignored (each is named at
      startup if set); CORK_REGISTRY and the material to talk to it
      (CORK_REGISTRY_CERT_DIR, the registry being where destroy and prune
      untag) are required, and a missing DOCKER_CERT_PATH is warned about;
      POST /update, POST /challenges/<id> and GET/POST /pins answer 409; a
      schema operation answers 409 naming any build that has not been
      handed over; and a launch with no worker registered fails instead of
      running locally.

HTTP API:
  corkd owns all state; every action goes through its API (the cork
  binary is a thin wrapper around it). In addition to the challenge, build,
  instance, worker, and schema endpoints, POST /update re-scans the
  challenge directory (body: {"path": "<dir>", "dry_run": false,
  "prune_old": false} — prune_old removes image generations displaced from
  rollback retention, on the build daemon and in the registry),
  GET /state dumps the full challenge/build/instance state,
  GET /version reports the server version and its build plane, GET/POST
  /pins list the base image pins and re-resolve them, and on an external
  build plane PUT /challenges/<id> takes a challenge and its builds handed
  over by whatever built them: the JSON {"challenge": <the GET /state
  element>, "pin_fingerprint": <the pin fingerprint the builds were made
  under, 0 without pins>}, and nothing else -- artifact bundles stay on the
  build plane that made them, and has_artifacts says what a build published
  rather than promising bytes. ?prune_old=true is update's --prune-old.
  Every build's identity is recomputed from those
  inputs and every image tag asked of the registry before anything is
  recorded (400 for a payload that contradicts itself, 409 for a tag the
  registry does not serve); the answer is update's, with the challenge as
  recorded. DELETE /challenges/<id> removes a challenge with no builds on
  record (409 while it has any: they go with their schema).

Workers:
  When docker workers are configured (GET/POST/PATCH/DELETE on /workers or
  cork worker-*), new instances are placed on them round robin,
  skipping any worker that is not reachable or is reporting overloaded;
  with none configured, corkd behaves
  as a single-host daemon using DOCKER_HOST (on a local build plane; an
  external one refuses the launch). Worker connections use the TLS
  material from DOCKER_CERT_PATH with the server name pinned to
  'academy-docker-worker' (the shared worker certificate), dockerd on port
  2376, and the telemetry agent on port 2136.

  A worker is ejected as unresponsive after CORK_WORKER_MAX_MISSES failed
  dockerd probes (30s by default) or a single hung/refused docker control
  call. That verdict reverses itself: the probe keeps running, and once the
  daemon answers again the worker reconnects and reconciles before rejoining
  placement, with its instances (their containers restart on their own). A
  reboot or a 'systemctl restart docker' therefore needs no operator action.
  Telemetry silence does not eject anything -- it moves the load axis to
  unknown, and unknown still takes placements.

  A PATCH of {"health": "down"} is the exception: it is asserted rather than
  observed, so no probe will lift it, and it is meant for a box about to be
  terminated. Recovery from it is POST /workers (or cork worker-add). When
  the box is terminated and recreated instead, DELETE it from /workers, which
  purges the worker and all of its instance records, and add the new one.
  Stops for instances on a worker that is down, or that the probes have given
  up on, clear the records and return success without touching docker.

  Whenever a worker is added, and for every worker at startup, the containers
  and cmgr-<id> networks cork created on it for instances it no longer records
  there (left behind by those stops, or by DELETE) are removed before it takes
  placements, so their host ports are free again. A daemon that cannot be
  reached at that point (still starting, say) is retried for
  CORK_WORKER_MAX_MISSES poll intervals before the worker is ejected; it is
  still probed after that, and still comes back on its own.

  That cleanup is not guaranteed. When it reaches the daemon but cannot
  finish, because a database read failed or the daemon refused a removal, it
  is retried for the same span and the worker then takes placements anyway,
  with an error naming it in the log. What is left holds its host ports, so a
  launch there may fail on a bind and be retried elsewhere until the box is
  reconciled again -- by its own recovery, a worker-add or a corkd start -- or
  docker-reaper removes the containers. The alternative, holding a whole box out of the
  fleet over one container its daemon will not remove, costs more.

  Under load a launch fails fast rather than queueing: it is refused at once
  when the launches already waiting on its worker would keep it waiting
  longer than CORK_WORKER_LAUNCH_WAIT, waits at most that long otherwise, and
  is refused as soon as its worker stops being reachable; all three answer 503
  with Retry-After so the platform's retry is placed afresh. A stop whose
  worker hangs mid-way clears the records once the worker is ejected and
  returns success, and so does one whose teardown times out against a daemon
  that is only slow; docker-reaper removes what it left. Stops wait
  for a teardown slot on their worker as long as it takes, since a stop must
  go through, so a deluge of them queues in corkd, bounded, rather than
  inside dockerd.

  The restart of a persistent instance during an update is exempt, since
  nothing retries it: it pulls the new image under a ceiling of five minutes,
  or CORK_WORKER_PULL_TIMEOUT when that is longer,
  while the old containers keep serving, then swaps them, waiting for its
  slot as long as it takes. One that cannot be restarted (its worker down,
  the pull or the start failed) is removed instead, like any stop on a down
  worker, reported as an error of the update, and relaunched through
  placement by the same update once the build's restarts are done. The
  schema converge (add-schema, update-schema) launches persistent instances
  under the same limits.

  Workers have two addresses: the private IP corkd dials, and an optional
  player-facing public address ("public" in the POST /workers body).
  Instance metadata reports the public one as "worker_public" (falling back
  to the private IP when unset).

  Note: The Docker client is configured via Docker's standard environment
      variables.  See https://docs.docker.com/engine/reference/commandline/cli/
      for specific details.

`, os.Args[0])
}

type ChallengeListElement struct {
	Id               cork.ChallengeId  `json:"id"`
	SourceChecksum   uint32            `json:"source_checksum"`
	MetadataChecksum uint32            `json:"metadata_checksum"`
	SolveScript      bool              `json:"solve_script"`
	DeliveryType     cork.DeliveryType `json:"delivery_type"`
}

func (s state) listHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	tags, ok := query["tags"]
	var challenges []*cork.ChallengeMetadata
	if !ok {
		challenges = s.mgr.ListChallenges()
	} else {
		challenges = s.mgr.SearchChallenges(tags)
	}

	respList := make([]ChallengeListElement, len(challenges))
	for i, challenge := range challenges {
		respList[i].Id = challenge.Id
		respList[i].SourceChecksum = challenge.SourceChecksum
		respList[i].MetadataChecksum = challenge.MetadataChecksum
		respList[i].SolveScript = challenge.SolveScript
		respList[i].DeliveryType = challenge.DeliveryType
	}
	body, err := json.Marshal(respList)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	w.Write(body)
}

type BuildChallengeRequest struct {
	FlagFormat string `json:"flag_format"`
	Seeds      []int  `json:"seeds"`
}

type InstanceStartRequest struct {
	UserId string            `json:"user_id"`
	Env    map[string]string `json:"env"`
}

func (s state) challengeHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Split(r.URL.Path, "/")
	pathLen := len(path)
	if len(path) < 2 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	chalStr := ""
	idx := pathLen - 1
	for idx >= 0 && path[idx] != "challenges" {
		chalStr = path[idx] + "/" + chalStr
		idx--
	}

	if idx < 0 || chalStr == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	challenge := cork.ChallengeId(chalStr[:len(chalStr)-1])

	var err error
	respCode := http.StatusOK
	var body []byte
	switch r.Method {
	case "GET":
		var meta *cork.ChallengeMetadata
		meta, err = s.mgr.GetChallengeMetadata(challenge)
		if err == nil {
			body, err = json.Marshal(meta)
		}
	case "POST":
		if s.refuseOnExternalBuildPlane(w) {
			return
		}
		var data []byte
		var buildReq BuildChallengeRequest
		data, err = ioutil.ReadAll(r.Body)

		if err == nil {
			err = json.Unmarshal(data, &buildReq)
		}

		var builds []*cork.BuildMetadata
		if err == nil {
			if buildReq.FlagFormat == "" {
				buildReq.FlagFormat = "flag{%s}"
			}
			builds, err = s.mgr.Build(challenge, buildReq.Seeds, buildReq.FlagFormat)
		}

		if err == nil {
			body, err = json.Marshal(builds)
		}
	case "PUT":
		s.handOverHandler(w, r, challenge)
		return
	case "DELETE":
		err = s.mgr.RemoveChallenge(challenge)
		respCode = http.StatusNoContent
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err != nil {
		respCode = http.StatusInternalServerError
		if _, ok := err.(*cork.UnknownIdentifierError); ok {
			respCode = http.StatusNotFound
		}
		if errors.Is(err, cork.ErrChallengeHasBuilds) {
			respCode = http.StatusConflict
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}

// HandOverResponse is what PUT /challenges/{id} answers: the verdict as
// POST /update reports one, the challenge as recorded (its builds with
// their ids), and the errors of builds that could not be committed once the
// writes had begun, with which the status is 500 rather than 200.
type HandOverResponse struct {
	UpdateResponse
	Challenge *cork.ChallengeMetadata `json:"challenge,omitempty"`
}

// handOverJSONLimit bounds the body of a hand-over, which is one challenge's
// metadata and image tags: large for what it carries, and nowhere near what
// a body would have to be to be something else.
const handOverJSONLimit = 16 << 20

// handOverHandler is PUT /challenges/{id}, the hand-over of a challenge and
// its builds from an external build plane (cork.HandOverChallenge): a JSON
// cork.HandOver body, and nothing else. Artifact bundles do not travel with
// it -- they stay on the build plane that made them, where what publishes
// them to players reads them -- so a hand-over is metadata and image tags,
// and its size is bounded by handOverJSONLimit alone. ?prune_old=true is
// update's --prune-old.
func (s state) handOverHandler(w http.ResponseWriter, r *http.Request, challenge cork.ChallengeId) {
	refuse := func(code int, msg string) {
		w.WriteHeader(code)
		w.Write([]byte(msg))
	}
	var handOver cork.HandOver
	if err := json.NewDecoder(io.LimitReader(r.Body, handOverJSONLimit)).Decode(&handOver); err != nil {
		refuse(http.StatusBadRequest, "invalid hand-over JSON: "+err.Error())
		return
	}
	options := cork.UpdateOptions{PruneOldImages: r.URL.Query().Get("prune_old") == "true"}

	updates, err := s.mgr.HandOverChallenge(challenge, &handOver, options)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, cork.ErrHandOverInvalid):
			code = http.StatusBadRequest
		case errors.Is(err, cork.ErrNotInRegistry), errors.Is(err, cork.ErrLocalBuildPlane):
			code = http.StatusConflict
		}
		refuse(code, err.Error())
		return
	}

	resp := HandOverResponse{UpdateResponse: UpdateResponse{
		Added:      challengeIds(updates.Added),
		Refreshed:  challengeIds(updates.Refreshed),
		Updated:    challengeIds(updates.Updated),
		Stale:      challengeIds(updates.Stale),
		Removed:    challengeIds(updates.Removed),
		Unmodified: challengeIds(updates.Unmodified),
		Errors:     make([]string, len(updates.Errors)),
	}}
	for i, updateErr := range updates.Errors {
		resp.Errors[i] = updateErr.Error()
	}
	for _, bucket := range [][]*cork.ChallengeMetadata{updates.Added, updates.Updated, updates.Refreshed, updates.Stale, updates.Unmodified} {
		if len(bucket) > 0 {
			resp.Challenge = bucket[0]
		}
	}
	body, err := json.Marshal(resp)
	if err != nil {
		refuse(http.StatusInternalServerError, err.Error())
		return
	}
	if len(updates.Errors) > 0 {
		w.WriteHeader(http.StatusInternalServerError)
	}
	w.Write(body)
}

func (s state) buildHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Split(r.URL.Path, "/")
	pathLen := len(path)

	// GET /builds/{id}/{artifact} was cmgr's artifact download, and cork does
	// not serve artifacts: a bundle stays on the build plane that made it and
	// reaches players from there. A path below a build is nothing here.
	if pathLen == 4 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if len(path) < 2 || path[pathLen-2] != "builds" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	buildInt, err := strconv.Atoi(path[pathLen-1])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(err.Error()))
	}

	build := cork.BuildId(buildInt)

	var body []byte
	var respCode int
	switch r.Method {
	case "GET":
		var meta *cork.BuildMetadata
		meta, err = s.mgr.GetBuildMetadata(build)
		respCode = http.StatusOK
		if err == nil {
			body, err = json.Marshal(meta)
		}
	case "POST":
		var instance cork.InstanceId
		envVars := make(map[string]string)
		if r.Body != nil {
			var req InstanceStartRequest
			err = json.NewDecoder(r.Body).Decode(&req)
			if err != nil && err != io.EOF {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("invalid request body: " + err.Error()))
				return
			}
			err = nil
			// The CMGR_ prefix here is the challenge contract (what a container
			// sees), not a daemon setting, so the rename leaves it alone.
			if req.Env != nil {
				for k, v := range req.Env {
					envVars["CMGR_"+k] = v
				}
			}
			if req.UserId != "" {
				envVars["CMGR_USER_ID"] = req.UserId
			}
		}

		instance, err = s.mgr.Start(build, envVars)
		respCode = http.StatusCreated

		var iMeta *cork.InstanceMetadata
		if err == nil {
			iMeta, err = s.mgr.GetInstanceMetadata(instance)
		}

		if err == nil {
			body, err = json.Marshal(iMeta)
		}
	case "DELETE":
		err = s.mgr.Destroy(build)
		respCode = http.StatusNoContent
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err != nil {
		var retry bool
		respCode, retry = errorResponse(err, retryableLaunch)
		if retry {
			w.Header().Set("Retry-After", "1")
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}

func (s state) instanceHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Split(r.URL.Path, "/")
	pathLen := len(path)
	if len(path) < 2 || path[pathLen-2] != "instances" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	instInt, err := strconv.Atoi(path[pathLen-1])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(err.Error()))
		return
	}

	instance := cork.InstanceId(instInt)

	var body []byte
	var respCode int
	switch r.Method {
	case "GET":
		var meta *cork.InstanceMetadata
		meta, err = s.mgr.GetInstanceMetadata(instance)
		respCode = http.StatusOK
		if err == nil {
			body, err = json.Marshal(meta)
		}
	case "DELETE":
		err = s.mgr.Stop(instance)
		// Idempotent delete: the instance may already be gone (pruned by the
		// TTL sweep, or cleared when its worker died). Callers only need to
		// know it no longer exists.
		if _, ok := err.(*cork.UnknownIdentifierError); ok {
			err = nil
		}
		respCode = http.StatusNoContent
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		var retry bool
		respCode, retry = errorResponse(err, retryableStop)
		if retry {
			w.Header().Set("Retry-After", "1")
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}

func (s state) existingSchemaHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Split(r.URL.Path, "/")
	pathLen := len(path)
	if len(path) < 2 || path[pathLen-2] != "schemas" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	schema := path[pathLen-1]

	var body []byte
	var err error
	var errStatus int
	respCode := http.StatusOK
	switch r.Method {
	case "GET":
		var meta []*cork.ChallengeMetadata
		meta, err = s.mgr.GetSchemaState(schema)
		if err == nil {
			body, err = json.Marshal(meta)
		}
	case "POST":
		var data []byte
		data, err = ioutil.ReadAll(r.Body)
		respCode = http.StatusNoContent

		var schemaDef *cork.Schema
		if err == nil {
			err = json.Unmarshal(data, &schemaDef)
		}

		if err == nil {
			if schemaDef.Name != schema {
				respCode = http.StatusBadRequest // Bad Request
				err = errors.New("mismatch between endpoint and schema name")
			} else {
				errs := s.mgr.UpdateSchema(schemaDef)
				if len(errs) > 0 {
					err = errors.Join(errs...)
					errStatus = schemaStatus(errs)
				}
			}
		}
	case "DELETE":
		// ?retire=false releases the schema without taking its images out
		// of the challenge registry: the build plane sends it when a schema
		// is moving to another orchestrator, whose hand-over resolves the
		// same content-addressed tags. Absent or anything else destroys,
		// which is what a removal is.
		retire := r.URL.Query().Get("retire") != "false"
		err = s.mgr.DeleteSchema(schema, retire)
		respCode = http.StatusNoContent
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err != nil {
		respCode = http.StatusInternalServerError
		if _, ok := err.(*cork.UnknownIdentifierError); ok {
			respCode = http.StatusNotFound
		}
		if errStatus != 0 {
			respCode = errStatus
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}

type UpdateRequest struct {
	Path   string `json:"path"`
	DryRun bool   `json:"dry_run"`
	// PruneOld additionally removes image generations that fall out of
	// rollback retention when a rebuild lands ({current, previous} are kept
	// per build), both on the build daemon and in the challenge registry.
	PruneOld bool `json:"prune_old"`
}

// ChallengeUpdates with ids instead of full metadata and marshalable errors.
type UpdateResponse struct {
	Added      []cork.ChallengeId `json:"added"`
	Refreshed  []cork.ChallengeId `json:"refreshed"`
	Updated    []cork.ChallengeId `json:"updated"`
	Stale      []cork.ChallengeId `json:"stale"`
	Removed    []cork.ChallengeId `json:"removed"`
	Unmodified []cork.ChallengeId `json:"unmodified"`
	Errors     []string           `json:"errors"`
}

func challengeIds(metas []*cork.ChallengeMetadata) []cork.ChallengeId {
	ids := make([]cork.ChallengeId, len(metas))
	for i, meta := range metas {
		ids[i] = meta.Id
	}
	return ids
}

func (s state) updateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.refuseOnExternalBuildPlane(w) {
		return
	}

	var req UpdateRequest
	if r.Body != nil {
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil && err != io.EOF {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid request body: " + err.Error()))
			return
		}
	}

	path := req.Path
	if path == "" {
		path = cork.Getenv(cork.DIR_ENV)
		if path == "" {
			path = "."
		}
	}

	// Synchronous: changed challenges are rebuilt (images + registry pushes)
	// before this returns, so large deploys need a generous client timeout.
	var updates *cork.ChallengeUpdates
	if req.DryRun {
		updates = s.mgr.DetectChanges(path)
	} else {
		updates = s.mgr.UpdateWithOptions(path, cork.UpdateOptions{PruneOldImages: req.PruneOld})
	}

	resp := UpdateResponse{
		Added:      challengeIds(updates.Added),
		Refreshed:  challengeIds(updates.Refreshed),
		Updated:    challengeIds(updates.Updated),
		Stale:      challengeIds(updates.Stale),
		Removed:    challengeIds(updates.Removed),
		Unmodified: challengeIds(updates.Unmodified),
		Errors:     make([]string, len(updates.Errors)),
	}
	for i, updateErr := range updates.Errors {
		resp.Errors[i] = updateErr.Error()
	}

	body, err := json.Marshal(resp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	w.Write(body)
}

func (s state) stateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	meta, err := s.mgr.DumpState(nil)
	var body []byte
	if err == nil {
		body, err = json.Marshal(meta)
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	w.Write(body)
}

func (s state) versionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := json.Marshal(map[string]string{"version": cork.Version(), "build_plane": s.mgr.BuildPlane()})
	w.Write(body)
}

// schemaStatus maps the errors a schema operation returns to one status:
// 409 when every one is a build not yet handed over
// (cork.ErrExternalBuildPlane), 404 when every one is an unknown
// identifier, 500 otherwise. A list that mixes either with a real failure
// is a failure, whatever else it holds: a 409 that hid a launch error would
// read as "hand over and retry" when the retry could not help.
func schemaStatus(errs []error) int {
	status := 0
	for _, err := range errs {
		code := http.StatusInternalServerError
		var unknown *cork.UnknownIdentifierError
		switch {
		case errors.Is(err, cork.ErrExternalBuildPlane):
			code = http.StatusConflict
		case errors.As(err, &unknown):
			code = http.StatusNotFound
		}
		if code == http.StatusInternalServerError || (status != 0 && status != code) {
			return http.StatusInternalServerError
		}
		status = code
	}
	if status == 0 {
		return http.StatusInternalServerError
	}
	return status
}

// refuseOnExternalBuildPlane answers 409 to a request only a local build
// plane can serve -- an update, a manual build, the pins -- and reports
// whether it did. The request is well-formed; this daemon is just not the
// one that builds (see cork.ErrExternalBuildPlane).
func (s state) refuseOnExternalBuildPlane(w http.ResponseWriter) bool {
	if s.mgr.BuildPlane() != cork.BuildPlaneExternal {
		return false
	}
	w.WriteHeader(http.StatusConflict)
	w.Write([]byte(cork.ErrExternalBuildPlane.Error()))
	return true
}

func (s state) schemaHandler(w http.ResponseWriter, r *http.Request) {
	var body []byte
	var err error
	var errStatus int
	respCode := http.StatusOK
	switch r.Method {
	case "GET":
		var schemaList []string
		schemaList, err = s.mgr.ListSchemas()
		if err == nil {
			body, err = json.Marshal(schemaList)
		}
	case "POST":
		var data []byte
		data, err = ioutil.ReadAll(r.Body)

		var schemaDef *cork.Schema
		if err == nil {
			err = json.Unmarshal(data, &schemaDef)
		}

		if err == nil {
			errs := s.mgr.CreateSchema(schemaDef)
			if len(errs) > 0 {
				err = errors.Join(errs...)
				errStatus = schemaStatus(errs)
			} else {
				respCode = http.StatusCreated
			}
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err != nil {
		respCode = http.StatusInternalServerError
		if _, ok := err.(*cork.UnknownIdentifierError); ok {
			respCode = http.StatusNotFound
		}
		if errStatus != 0 {
			respCode = errStatus
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}
