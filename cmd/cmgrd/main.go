package main

import (
	"archive/tar"
	"compress/gzip"
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

	"github.com/picoCTF/cmgr/cmgr"
)

type state struct {
	mgr *cmgr.Manager
}

var artifact_dir string

func main() {
	var iface string
	var port int
	var help bool
	var version bool
	flag.IntVar(&port, "port", 4200, "listening port for cmgrd")
	flag.StringVar(&iface, "address", "", "listening address for cmgrd")
	flag.BoolVar(&help, "help", false, "display usage information")
	flag.BoolVar(&version, "version", false, "display version information")
	flag.Parse()

	if version {
		fmt.Printf("Version: %s\n", cmgr.Version())
		os.Exit(0)
	}

	if help {
		printUsage()
		os.Exit(0)
	}

	artifact_dir, _ = os.LookupEnv(cmgr.ARTIFACT_DIR_ENV)
	if artifact_dir == "" {
		artifact_dir = "."
	}
	mgr := cmgr.NewManager(cmgr.INFO)
	if mgr == nil {
		log.Fatal("failed to initialize cmgr library")
	}

	// cmgrd is the sole owner of the database and the docker/registry state:
	// every action (deploys, builds, instance lifecycle, worker management)
	// goes through its HTTP API. New instances are placed on the configured
	// workers (round robin, skipping overloaded/down ones).
	mgr.EnableWorkerPlacement()

	s := state{mgr: mgr}

	http.HandleFunc("/challenges", s.listHandler)
	http.HandleFunc("/challenges/", s.challengeHandler)
	http.HandleFunc("/builds/", s.buildHandler)
	http.HandleFunc("/instances/", s.instanceHandler)
	http.HandleFunc("/workers", s.workersHandler)
	http.HandleFunc("/workers/", s.workerHandler)
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
  CMGR_DB - path to cmgr's database file (defaults to 'cmgr.db')

  CMGR_DIR - directory containing all challenges (defaults to '.')

  CMGR_ARTIFACT_DIR - directory for storing artifact bundles (defaults to '.')

  CMGR_LOGGING - controls the verbosity of the internal logging infrastructure
      and should be one of the following: debug, info, warn, error, or disabled
      (defaults to 'info')

  CMGR_PORTS - the range of ports that are dedicated for serving challenges;
      cmgr will assume that it fully owns these ports and nothing else will
      try to use them (i.e., not in ephemeral range or overlapping with a
      service running on the host); format is '1000-1000'

  CMGR_INTERFACE - the host interface/address to which published challenge
      ports should be bound (defaults to '0.0.0.0'); if the specified interface
      does not exist on the host running the Docker daemon, Docker will silently
      ignore this value and instead bind to the loopback address

  CMGR_PRUNE_AGE - the maximum age for on-demand challenge instances; old
      instances are automatically pruned from the database (defaults to '1h');
      set to '0' to disable automatic pruning.
  CMGR_DB_WAL - controls whether SQLite WAL journaling mode is enabled;
      on by default for improved throughput under high concurrency;
      creates <db>-wal and <db>-shm sidecar files; do NOT use on network-mounted
      filesystems (NFS, SMB) as this may cause corruption; set to 'false'
      to disable.

  CMGR_CONCURRENT_LAUNCHES - the maximum number of concurrent container
      launches allowed (defaults to 2); allowed values are 1 or 2.

  CMGR_REGISTRY - the docker registry holding built challenge images; when
      set, images are pulled from it before each instance start (must match
      the value used by cmgr when building).

HTTP API:
  cmgrd owns all state; every action goes through its API (the cmgrd-cli
  binary is a thin wrapper around it). In addition to the challenge, build,
  instance, worker, and schema endpoints, POST /update re-scans the
  challenge directory (body: {"path": "<dir>", "dry_run": false}),
  GET /state dumps the full challenge/build/instance state, and
  GET /version reports the server version.

Workers:
  When docker workers are configured (POST/DELETE/GET on /workers or
  cmgrd-cli worker-*), new instances are placed on them round robin,
  skipping overloaded and down workers; with none configured, cmgrd behaves
  as a single-host daemon using DOCKER_HOST. Worker connections use the TLS
  material from DOCKER_CERT_PATH with the server name pinned to
  'academy-docker-worker' (the shared worker certificate), dockerd on port
  2376, and the telemetry agent on port 2136.

  A worker goes down (sticky) after 30s of telemetry silence or a single
  hung/refused docker control call; recovery is re-adding it (POST /workers
  or cmgrd-cli worker-add). Stops for instances on a down worker clear the
  records and return success without touching docker. DELETE on /workers
  purges the worker and all of its instance records.

  Workers have two addresses: the private IP cmgrd dials, and an optional
  player-facing public address ("public" in the POST /workers body).
  Instance metadata reports the public one as "worker_public" (falling back
  to the private IP when unset).

  Note: The Docker client is configured via Docker's standard environment
      variables.  See https://docs.docker.com/engine/reference/commandline/cli/
      for specific details.

`, os.Args[0])
}

type ChallengeListElement struct {
	Id               cmgr.ChallengeId  `json:"id"`
	SourceChecksum   uint32            `json:"source_checksum"`
	MetadataChecksum uint32            `json:"metadata_checksum"`
	SolveScript      bool              `json:"solve_script"`
	DeliveryType     cmgr.DeliveryType `json:"delivery_type"`
}

func (s state) listHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	tags, ok := query["tags"]
	var challenges []*cmgr.ChallengeMetadata
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

	challenge := cmgr.ChallengeId(chalStr[:len(chalStr)-1])

	var err error
	respCode := http.StatusOK
	var body []byte
	switch r.Method {
	case "GET":
		var meta *cmgr.ChallengeMetadata
		meta, err = s.mgr.GetChallengeMetadata(challenge)
		if err == nil {
			body, err = json.Marshal(meta)
		}
	case "POST":
		var data []byte
		var buildReq BuildChallengeRequest
		data, err = ioutil.ReadAll(r.Body)

		if err == nil {
			err = json.Unmarshal(data, &buildReq)
		}

		var builds []*cmgr.BuildMetadata
		if err == nil {
			if buildReq.FlagFormat == "" {
				buildReq.FlagFormat = "flag{%s}"
			}
			builds, err = s.mgr.Build(challenge, buildReq.Seeds, buildReq.FlagFormat)
		}

		if err == nil {
			body, err = json.Marshal(builds)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err != nil {
		respCode = http.StatusInternalServerError
		if _, ok := err.(*cmgr.UnknownIdentifierError); ok {
			respCode = http.StatusNotFound
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}

func (s state) buildHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Split(r.URL.Path, "/")
	pathLen := len(path)

	if pathLen == 4 {
		s.artifactsHandler(w, r)
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

	build := cmgr.BuildId(buildInt)

	var body []byte
	var respCode int
	switch r.Method {
	case "GET":
		var meta *cmgr.BuildMetadata
		meta, err = s.mgr.GetBuildMetadata(build)
		respCode = http.StatusOK
		if err == nil {
			body, err = json.Marshal(meta)
		}
	case "POST":
		var instance cmgr.InstanceId
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

		var iMeta *cmgr.InstanceMetadata
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
		respCode = http.StatusInternalServerError
		if _, ok := err.(*cmgr.UnknownIdentifierError); ok {
			respCode = http.StatusNotFound
		}
		// Every worker was skipped: overloaded somewhere means retryable,
		// everything down means a real failure.
		if errors.Is(err, cmgr.ErrAllWorkersOverloaded) {
			respCode = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "1")
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}

func (s state) artifactsHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Split(r.URL.Path, "/")
	pathLen := len(path)
	if pathLen < 4 || path[pathLen-3] != "builds" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	buildInt, err := strconv.Atoi(path[pathLen-2])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(err.Error()))
		return
	}

	build := cmgr.BuildId(buildInt)
	meta, err := s.mgr.GetBuildMetadata(build)
	_, ok := err.(*cmgr.UnknownIdentifierError)
	if ok || (err != nil && !meta.HasArtifacts) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	f, err := os.Open(fmt.Sprintf("%s/%d.tar.gz", artifact_dir, build))

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	defer f.Close()

	if path[pathLen-1] == "artifacts.tar.gz" {
		io.Copy(w, f)
		return
	}
	srcGz, err := gzip.NewReader(f)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	defer srcGz.Close()
	srcTar := tar.NewReader(srcGz)

	var h *tar.Header
	for h, err = srcTar.Next(); err == nil; h, err = srcTar.Next() {
		if h.Name == path[pathLen-1] {
			io.Copy(w, srcTar)
			return
		}
	}

	if err == io.EOF {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(err.Error()))
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

	instance := cmgr.InstanceId(instInt)

	var body []byte
	var respCode int
	switch r.Method {
	case "GET":
		var meta *cmgr.InstanceMetadata
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
		if _, ok := err.(*cmgr.UnknownIdentifierError); ok {
			err = nil
		}
		respCode = http.StatusNoContent
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		respCode = http.StatusInternalServerError
		if _, ok := err.(*cmgr.UnknownIdentifierError); ok {
			respCode = http.StatusNotFound
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
	respCode := http.StatusOK
	switch r.Method {
	case "GET":
		var meta []*cmgr.ChallengeMetadata
		meta, err = s.mgr.GetSchemaState(schema)
		if err == nil {
			body, err = json.Marshal(meta)
		}
	case "POST":
		var data []byte
		data, err = ioutil.ReadAll(r.Body)
		respCode = http.StatusNoContent

		var schemaDef *cmgr.Schema
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
					err = fmt.Errorf("%v", errs)
				}
			}
		}
	case "DELETE":
		err = s.mgr.DeleteSchema(schema)
		respCode = http.StatusNoContent
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err != nil {
		respCode = http.StatusInternalServerError
		if _, ok := err.(*cmgr.UnknownIdentifierError); ok {
			respCode = http.StatusNotFound
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}

type UpdateRequest struct {
	Path   string `json:"path"`
	DryRun bool   `json:"dry_run"`
}

// ChallengeUpdates with ids instead of full metadata and marshalable errors.
type UpdateResponse struct {
	Added      []cmgr.ChallengeId `json:"added"`
	Refreshed  []cmgr.ChallengeId `json:"refreshed"`
	Updated    []cmgr.ChallengeId `json:"updated"`
	Removed    []cmgr.ChallengeId `json:"removed"`
	Unmodified []cmgr.ChallengeId `json:"unmodified"`
	Errors     []string           `json:"errors"`
}

func challengeIds(metas []*cmgr.ChallengeMetadata) []cmgr.ChallengeId {
	ids := make([]cmgr.ChallengeId, len(metas))
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
		path = os.Getenv(cmgr.DIR_ENV)
		if path == "" {
			path = "."
		}
	}

	// Synchronous: changed challenges are rebuilt (images + registry pushes)
	// before this returns, so large deploys need a generous client timeout.
	var updates *cmgr.ChallengeUpdates
	if req.DryRun {
		updates = s.mgr.DetectChanges(path)
	} else {
		updates = s.mgr.Update(path)
	}

	resp := UpdateResponse{
		Added:      challengeIds(updates.Added),
		Refreshed:  challengeIds(updates.Refreshed),
		Updated:    challengeIds(updates.Updated),
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
	body, _ := json.Marshal(map[string]string{"version": cmgr.Version()})
	w.Write(body)
}

func (s state) schemaHandler(w http.ResponseWriter, r *http.Request) {
	var body []byte
	var err error
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

		var schemaDef *cmgr.Schema
		if err == nil {
			err = json.Unmarshal(data, &schemaDef)
		}

		if err == nil {
			errs := s.mgr.CreateSchema(schemaDef)
			if len(errs) > 0 {
				err = fmt.Errorf("%v", errs)
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
		if _, ok := err.(*cmgr.UnknownIdentifierError); ok {
			respCode = http.StatusNotFound
		}
		body = []byte(err.Error())
	}

	w.WriteHeader(respCode)
	w.Write(body)
}
