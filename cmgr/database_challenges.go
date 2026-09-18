package cmgr

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Columns for list/search results: the light-weight subset plus what
// deriveDeliveryType needs (challenge type + published port count), so every
// ChallengeMetadata leaving these paths carries a valid delivery type.
const listChallengeColumns = `id, name, path, sourcechecksum, metadatachecksum, solvescript, challengetype,
	(SELECT COUNT(*) FROM portNames WHERE portNames.challenge = challenges.id) AS portcount`

type listChallengeRow struct {
	ChallengeMetadata
	PortCount int `db:"portcount"`
}

func listRowsToMetadata(rows []listChallengeRow) []*ChallengeMetadata {
	metadata := make([]*ChallengeMetadata, len(rows))
	for i := range rows {
		md := rows[i].ChallengeMetadata
		md.DeliveryType = deriveDeliveryType(md.ChallengeType, rows[i].PortCount)
		metadata[i] = &md
	}
	return metadata
}

// Gets just the ID and checksum for all known challenges
func (m *Manager) listChallenges() ([]*ChallengeMetadata, error) {
	rows := []listChallengeRow{}
	err := m.db.Select(&rows, "SELECT "+listChallengeColumns+" FROM challenges ORDER BY id;")
	return listRowsToMetadata(rows), err
}

func (m *Manager) searchChallenges(tags []string) ([]*ChallengeMetadata, error) {
	if len(tags) == 0 {
		return m.listChallenges()
	}

	interfaceTags := make([]interface{}, len(tags))
	for i, tag := range tags {
		interfaceTags[i] = strings.ReplaceAll(tag, "*", "%")
	}
	tagBaseQuery := "SELECT challenge FROM tags WHERE tag LIKE ?"
	subQuery := "(" +
		tagBaseQuery +
		strings.Repeat(" INTERSECT "+tagBaseQuery, len(tags)-1) +
		")"
	query := fmt.Sprintf("SELECT %s FROM challenges WHERE id IN %s ORDER BY id;", listChallengeColumns, subQuery)
	rows := []listChallengeRow{}
	err := m.db.Select(&rows, query, interfaceTags...)

	return listRowsToMetadata(rows), err
}

func (m *Manager) lookupChallengeMetadata(challenge ChallengeId) (*ChallengeMetadata, error) {
	metadata := new(ChallengeMetadata)
	txn := m.db.MustBegin()

	err := txn.Get(metadata, "SELECT * FROM challenges WHERE id=?", challenge)
	if isEmptyQueryError(err) {
		err = unknownChallengeIdError(challenge)
	}

	if err == nil {
		err = txn.Select(&metadata.Hints, "SELECT hint FROM hints WHERE challenge=? ORDER BY idx", challenge)
	}

	if err == nil {
		err = txn.Select(&metadata.Tags, "SELECT tag FROM tags WHERE challenge=?", challenge)
	}

	if err == nil {
		err = txn.Select(&metadata.Hosts, "SELECT name, target FROM hosts WHERE challenge=?", challenge)
	}

	ports := []struct {
		Name string
		Host string
		Port int
	}{}
	if err == nil {
		err = txn.Select(&ports, "SELECT name, host, port FROM portNames WHERE challenge=?", challenge)
	}

	metadata.PortMap = make(map[string]PortInfo)
	for _, port := range ports {
		metadata.PortMap[port.Name] = PortInfo{port.Host, port.Port}
	}

	// Derived from the same port data exposed as PortMap (plus the challenge
	// type) so the reported delivery type can never disagree with the
	// instance-launch decision that reads it.
	metadata.DeliveryType = deriveDeliveryType(metadata.ChallengeType, len(metadata.PortMap))

	attributes := []struct {
		Key   string
		Value string
	}{}
	if err == nil {
		err = txn.Select(&attributes, "SELECT key, value FROM attributes WHERE challenge=?", challenge)
	}

	metadata.Attributes = make(map[string]string)
	for _, attr := range attributes {
		metadata.Attributes[attr.Key] = attr.Value
	}

	networkOptions := new(NetworkOptions)
	// Note: there are currently no network-level challenge options, but they will be loaded here if added in the future.

	// if err == nil {
	// 	err = txn.Get(networkOptions, "SELECT '' FROM networkOptions WHERE challenge=?", challenge)
	// }
	metadata.ChallengeOptions.NetworkOptions = *networkOptions

	containerOptions := new([]dbContainerOptions)
	if err == nil {
		err = txn.Select(containerOptions, "SELECT host, init, cpus, memory, ulimits, pidslimit, readonlyrootfs, droppedcaps, nonewprivileges, diskquota, cgroupparent, capimmutable, seccomp FROM containerOptions WHERE challenge=?", challenge)
	}
	for _, dbOpts := range *containerOptions {
		var cOpts ContainerOptions
		cOpts, err = newFromDbContainerOptions(dbOpts)
		if err != nil {
			m.log.errorf("could not decode container options for '%s' (host '%s'): %s", challenge, dbOpts.Host, err)
			break
		}
		if metadata.ChallengeOptions.Overrides == nil {
			metadata.ChallengeOptions.Overrides = make(map[string]ContainerOptions)
		}
		metadata.ChallengeOptions.Overrides[dbOpts.Host] = cOpts
	}
	metadata.ChallengeOptions.ContainerOptions = metadata.ChallengeOptions.Overrides[""]

	if err == nil {
		err = txn.Commit()
		if err != nil {
			m.log.errorf("failed to commit read-only transaction: %s", err)
		}
	} else {
		m.log.errorf("read of database failed: %s", err)
		closeErr := txn.Rollback()
		if closeErr != nil {
			m.log.errorf("rollback failed: %s", err)
			err = closeErr
		}
	}

	return metadata, err
}

// Adds the discovered challenges to the database
func (m *Manager) addChallenges(addedChallenges []*ChallengeMetadata) []error {
	errs := []error{}
	for _, metadata := range addedChallenges {
		txn := m.db.MustBegin()

		_, err := txn.NamedExec(challengeInsertQuery, metadata)
		if err != nil {
			m.log.error(err)
			err = txn.Rollback()
			if err != nil { // If rollback fails, we're in trouble.
				m.log.error(err)
				return append(errs, err)
			}
			continue
		}

		for i, hint := range metadata.Hints {
			_, err = txn.Exec("INSERT INTO hints(challenge, idx, hint) VALUES (?, ?, ?);",
				metadata.Id,
				i,
				hint)

			if err != nil {
				m.log.error(err)
				err = txn.Rollback()
				if err != nil { // If rollback fails, we're in trouble.
					m.log.error(err)
					return append(errs, err)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		for _, tag := range metadata.Tags {
			_, err = txn.Exec("INSERT INTO tags(challenge, tag) VALUES (?, ?);",
				metadata.Id,
				tag)

			if err != nil {
				m.log.error(err)
				err = txn.Rollback()
				if err != nil { // If rollback fails, we're in trouble.
					m.log.error(err)
					return append(errs, err)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		for k, v := range metadata.Attributes {
			_, err = txn.Exec("INSERT INTO attributes(challenge, key, value) VALUES (?, ?, ?);",
				metadata.Id,
				k,
				v)

			if err != nil {
				m.log.error(err)
				err = txn.Rollback()
				if err != nil { // If rollback fails, we're in trouble.
					m.log.error(err)
					return append(errs, err)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		for i, host := range metadata.Hosts {
			m.log.debugf("%s: %v", metadata.Id, host)
			_, err = txn.Exec("INSERT INTO hosts(challenge, name, idx, target) VALUES (?, ?, ?, ?);",
				metadata.Id,
				host.Name,
				i,
				host.Target)

			if err != nil {
				m.log.error(err)
				err = txn.Rollback()
				if err != nil { // If rollback fails, we're in trouble.
					m.log.error(err)
					return append(errs, err)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		for k, v := range metadata.PortMap {
			m.log.debugf("%s: %v", metadata.Id, v)
			_, err = txn.Exec("INSERT INTO portNames(challenge, name, host, port) VALUES (?, ?, ?, ?);",
				metadata.Id,
				k,
				v.Host,
				v.Port)

			if err != nil {
				m.log.error(err)
				err = txn.Rollback()
				if err != nil { // If rollback fails, we're in trouble.
					m.log.error(err)
					return append(errs, err)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		// Note: there are currently no network-level challenge options, but they will be saved here if added in the future.

		// m.log.debugf("%s: %v", metadata.Id, metadata.ChallengeOptions.NetworkOptions)
		// _, err = txn.Exec("INSERT INTO networkOptions(challenge) VALUES (?);",
		// 	metadata.Id,
		// )
		// if err != nil {
		// 	m.log.error(err)
		// 	err = txn.Rollback()
		// 	if err != nil { // If rollback fails, we're in trouble.
		// 		m.log.error(err)
		// 		return append(errs, err)
		// 	}
		// }
		// if err != nil {
		// 	continue
		// }

		optsFailed := false
		for host, opts := range metadata.ChallengeOptions.Overrides {
			host_str := ""
			if host != "" {
				host_str = fmt.Sprintf(" (%s)", host)
			}
			dbOpts, optErr := opts.toDbContainerOptions()
			if optErr == nil {
				m.log.debugf("%s%s: %v", metadata.Id, host_str, dbOpts)
				_, optErr = txn.Exec("INSERT INTO containerOptions(challenge, host, init, cpus, memory, ulimits, pidslimit, readonlyrootfs, droppedcaps, nonewprivileges, diskquota, cgroupparent, capimmutable, seccomp) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);",
					metadata.Id,
					host,
					dbOpts.Init,
					dbOpts.Cpus,
					dbOpts.Memory,
					dbOpts.Ulimits,
					dbOpts.PidsLimit,
					dbOpts.ReadonlyRootfs,
					dbOpts.DroppedCaps,
					dbOpts.NoNewPrivileges,
					dbOpts.DiskQuota,
					dbOpts.CgroupParent,
					dbOpts.CapImmutable,
					dbOpts.Seccomp)
			}
			if optErr != nil {
				m.log.error(optErr)
				errs = append(errs, optErr)
				if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
					m.log.error(rbErr)
					return append(errs, rbErr)
				}
				optsFailed = true
				break
			}
		}
		if optsFailed {
			continue
		}

		if err := txn.Commit(); err != nil { // It's undocumented what this means...
			m.log.error(err)
			errs = append(errs, err)
		}
	}
	return errs
}

// rotatedPrevChecksum returns the rollback generation to retain after a
// rebuild. When the content changed (newChecksum != oldChecksum) the generation
// just replaced (oldChecksum) becomes the new rollback target; otherwise the
// existing target (currentPrev) is left untouched, so a no-op rebuild does not
// disturb retention.
func rotatedPrevChecksum(oldChecksum, newChecksum, currentPrev uint32) uint32 {
	if newChecksum != oldChecksum {
		return oldChecksum
	}
	return currentPrev
}

// displacedPruneCandidate reports whether a rebuild displaced an image
// generation that '--prune-old' should untag: the content actually changed
// (newChecksum != oldChecksum), there was a prior rollback generation to
// displace (displaced != 0), and that displaced generation is not the same
// content as the just-rebuilt one. The last clause guards an A->B->A
// flip-flop, where the displaced generation equals the new current generation
// and must therefore be kept, not pruned.
func displacedPruneCandidate(oldChecksum, newChecksum, displaced uint32) bool {
	return newChecksum != oldChecksum && displaced != 0 && displaced != newChecksum
}

// updateChallenges persists each challenge's metadata and then, when
// selectBuilds is given, rebuilds the builds it selects (see rebuildBuilds):
// allBuildIds for a source change, staleBuildIds to bring back whatever an
// earlier rebuild left at a previous generation, nil for a metadata-only
// refresh. A challenge whose metadata fails to persist is skipped before any
// rebuild, so a rebuild never runs against a row that does not match the tree.
func (m *Manager) updateChallenges(updatedChallenges []*ChallengeMetadata, selectBuilds func(*ChallengeMetadata) ([]BuildId, error), pruneOldImages bool) []error {
	errs := []error{}
	for _, metadata := range updatedChallenges {
		txn := m.db.MustBegin()

		_, err := txn.NamedExec(challengeUpdateQuery, metadata)
		if err != nil {
			m.log.error(err)
			errs = append(errs, err)
			if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
				m.log.error(rbErr)
				return append(errs, rbErr)
			}
			continue
		}

		_, err = txn.Exec("DELETE FROM hints WHERE challenge = ?;", metadata.Id)

		if err != nil {
			m.log.error(err)
			errs = append(errs, err)
			if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
				m.log.error(rbErr)
				return append(errs, rbErr)
			}
			continue
		}
		for i, hint := range metadata.Hints {

			_, err = txn.Exec("INSERT INTO hints(challenge, idx, hint) VALUES (?, ?, ?);",
				metadata.Id,
				i,
				hint)

			if err != nil {
				m.log.error(err)
				errs = append(errs, err)
				if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
					m.log.error(rbErr)
					return append(errs, rbErr)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		_, err = txn.Exec("DELETE FROM tags WHERE challenge = ?;", metadata.Id)

		if err != nil {
			m.log.error(err)
			errs = append(errs, err)
			if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
				m.log.error(rbErr)
				return append(errs, rbErr)
			}
			continue
		}
		for _, tag := range metadata.Tags {

			_, err = txn.Exec("INSERT INTO tags(challenge, tag) VALUES (?, ?);",
				metadata.Id,
				tag)

			if err != nil {
				m.log.error(err)
				errs = append(errs, err)
				if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
					m.log.error(rbErr)
					return append(errs, rbErr)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		_, err = txn.Exec("DELETE FROM attributes WHERE challenge = ?;", metadata.Id)

		if err != nil {
			m.log.error(err)
			errs = append(errs, err)
			if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
				m.log.error(rbErr)
				return append(errs, rbErr)
			}
			continue
		}
		for k, v := range metadata.Attributes {

			_, err = txn.Exec("INSERT INTO attributes(challenge, key, value) VALUES (?, ?, ?);",
				metadata.Id,
				k,
				v)

			if err != nil {
				m.log.error(err)
				errs = append(errs, err)
				if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
					m.log.error(rbErr)
					return append(errs, rbErr)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		_, err = txn.Exec("DELETE FROM hosts WHERE challenge = ?;", metadata.Id)

		if err != nil {
			m.log.error(err)
			errs = append(errs, err)
			if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
				m.log.error(rbErr)
				return append(errs, rbErr)
			}
			continue
		}

		for i, host := range metadata.Hosts {
			_, err = txn.Exec("INSERT INTO hosts(challenge, name, idx, target) VALUES (?, ?, ?, ?);",
				metadata.Id,
				host.Name,
				i,
				host.Target)

			if err != nil {
				m.log.error(err)
				errs = append(errs, err)
				if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
					m.log.error(rbErr)
					return append(errs, rbErr)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		_, err = txn.Exec("DELETE FROM portNames WHERE challenge = ?;", metadata.Id)

		if err != nil {
			m.log.error(err)
			errs = append(errs, err)
			if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
				m.log.error(rbErr)
				return append(errs, rbErr)
			}
			continue
		}

		for k, v := range metadata.PortMap {
			_, err = txn.Exec("INSERT INTO portNames(challenge, name, host, port) VALUES (?, ?, ?, ?);",
				metadata.Id,
				k,
				v.Host,
				v.Port)

			if err != nil {
				m.log.error(err)
				errs = append(errs, err)
				if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
					m.log.error(rbErr)
					return append(errs, rbErr)
				}
				break
			}
		}
		if err != nil {
			continue
		}

		// Note: there are currently no network-level challenge options, but they would be updated here if added in the future.

		// _, err = txn.Exec("DELETE FROM networkOptions WHERE challenge = ?;", metadata.Id)

		// if err != nil {
		// 	m.log.error(err)
		// 	err = txn.Rollback()
		// 	if err != nil { // If rollback fails, we're in trouble.
		// 		m.log.error(err)
		// 		return append(errs, err)
		// 	}
		// 	continue
		// }

		// _, err = txn.Exec("INSERT INTO networkOptions(challenge) VALUES (?);",
		// 	metadata.Id)
		// if err != nil {
		// 	m.log.error(err)
		// 	err = txn.Rollback()
		// 	if err != nil { // If rollback fails, we're in trouble.
		// 		m.log.error(err)
		// 		return append(errs, err)
		// 	}
		// }
		// if err != nil {
		// 	continue
		// }

		_, err = txn.Exec("DELETE FROM containerOptions WHERE challenge = ?;", metadata.Id)

		if err != nil {
			m.log.error(err)
			errs = append(errs, err)
			if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
				m.log.error(rbErr)
				return append(errs, rbErr)
			}
			continue
		}

		optsFailed := false
		for host, opts := range metadata.ChallengeOptions.Overrides {
			dbOpts, optErr := opts.toDbContainerOptions()
			if optErr == nil {
				_, optErr = txn.Exec("INSERT INTO containerOptions(challenge, host, init, cpus, memory, ulimits, pidslimit, readonlyrootfs, droppedcaps, nonewprivileges, diskquota, cgroupparent, capimmutable, seccomp) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);",
					metadata.Id,
					host,
					dbOpts.Init,
					dbOpts.Cpus,
					dbOpts.Memory,
					dbOpts.Ulimits,
					dbOpts.PidsLimit,
					dbOpts.ReadonlyRootfs,
					dbOpts.DroppedCaps,
					dbOpts.NoNewPrivileges,
					dbOpts.DiskQuota,
					dbOpts.CgroupParent,
					dbOpts.CapImmutable,
					dbOpts.Seccomp)
			}
			if optErr != nil {
				m.log.error(optErr)
				errs = append(errs, optErr)
				if rbErr := txn.Rollback(); rbErr != nil { // If rollback fails, we're in trouble.
					m.log.error(rbErr)
					return append(errs, rbErr)
				}
				optsFailed = true
				break
			}
		}
		if optsFailed {
			continue
		}

		if err := txn.Commit(); err != nil { // It's undocumented what this means...
			m.log.error(err)
			errs = append(errs, err)
			continue // next challenge
		}

		if selectBuilds != nil {
			buildIds, err := selectBuilds(metadata)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			errs = append(errs, m.rebuildBuilds(metadata, buildIds, pruneOldImages)...)
		}
	}
	return errs
}

func (m *Manager) removeChallenges(removedChallenges []*ChallengeMetadata) error {
	txn := m.db.MustBegin()
	for _, metadata := range removedChallenges {
		// This should throw an error and cause a rollback when builds exist for
		// a challenge we are removing.
		_, err := txn.Exec("DELETE FROM challenges WHERE id = ?;", metadata.Id)
		if err != nil {
			m.log.error(err)
			rbErr := txn.Rollback()
			if rbErr != nil { // If rollback fails, we're in trouble.
				m.log.error(rbErr)
				return rbErr
			}
			return err
		}
	}

	if err := txn.Commit(); err != nil { // It's undocumented what this means...
		m.log.error(err)
		return err
	}

	return nil
}

// Database representation of ContainerOptions
// List-based options are serialized as JSON strings
type dbContainerOptions struct {
	Host            string
	Init            bool
	Cpus            string
	Memory          string
	Ulimits         string
	PidsLimit       int64
	ReadonlyRootfs  bool
	DroppedCaps     string
	NoNewPrivileges bool
	DiskQuota       string
	CgroupParent    string
	CapImmutable    bool
	Seccomp         string
}

func newFromDbContainerOptions(dbOpts dbContainerOptions) (ContainerOptions, error) {
	cOpts := ContainerOptions{}

	cOpts.Init = dbOpts.Init

	cOpts.Cpus = dbOpts.Cpus

	cOpts.Memory = dbOpts.Memory

	ulimits := make([]string, 0)
	err := json.Unmarshal([]byte(dbOpts.Ulimits), &ulimits)
	if err != nil {
		return cOpts, err
	}
	cOpts.Ulimits = ulimits

	cOpts.PidsLimit = dbOpts.PidsLimit

	cOpts.ReadonlyRootfs = dbOpts.ReadonlyRootfs

	droppedCaps := make([]string, 0)
	err = json.Unmarshal([]byte(dbOpts.DroppedCaps), &droppedCaps)
	if err != nil {
		return cOpts, err
	}
	cOpts.DroppedCaps = droppedCaps

	cOpts.NoNewPrivileges = dbOpts.NoNewPrivileges

	cOpts.DiskQuota = dbOpts.DiskQuota

	cOpts.CgroupParent = dbOpts.CgroupParent
	cOpts.CapImmutable = dbOpts.CapImmutable

	cOpts.Seccomp, err = unmarshalSeccompOptions(dbOpts.Seccomp)
	if err != nil {
		return cOpts, err
	}

	return cOpts, nil
}

func (cOpts ContainerOptions) toDbContainerOptions() (dbContainerOptions, error) {
	dbOpts := dbContainerOptions{}

	dbOpts.Init = cOpts.Init

	dbOpts.Cpus = cOpts.Cpus

	dbOpts.Memory = cOpts.Memory

	ulimitsBytes, err := json.Marshal(cOpts.Ulimits)
	if err != nil {
		return dbOpts, err
	}
	ulimits := string(ulimitsBytes)
	dbOpts.Ulimits = ulimits

	dbOpts.PidsLimit = cOpts.PidsLimit

	dbOpts.ReadonlyRootfs = cOpts.ReadonlyRootfs

	droppedCapsBytes, err := json.Marshal(cOpts.DroppedCaps)
	if err != nil {
		return dbOpts, err
	}
	droppedCaps := string(droppedCapsBytes)
	dbOpts.DroppedCaps = droppedCaps

	dbOpts.NoNewPrivileges = cOpts.NoNewPrivileges

	dbOpts.DiskQuota = cOpts.DiskQuota

	dbOpts.CgroupParent = cOpts.CgroupParent
	dbOpts.CapImmutable = cOpts.CapImmutable

	dbOpts.Seccomp, err = marshalSeccompOptions(cOpts.Seccomp)
	if err != nil {
		return dbOpts, err
	}

	return dbOpts, nil
}

const (
	challengeInsertQuery string = `
	INSERT INTO challenges (
		id,
		name,
		namespace,
		challengetype,
		description,
		details,
		sourcechecksum,
		metadatachecksum,
		path,
		solvescript,
		templatable,
		maxusers,
		category,
		points
	)
	VALUES (
		:id,
		:name,
		:namespace,
		:challengetype,
		:description,
		:details,
		:sourcechecksum,
		:metadatachecksum,
		:path,
		:solvescript,
		:templatable,
		:maxusers,
		:category,
		:points
	);`

	challengeUpdateQuery string = `
	UPDATE challenges SET
	    name = :name,
		challengetype = :challengetype,
		description = :description,
		details = :details,
		sourcechecksum = :sourcechecksum,
		metadatachecksum = :metadatachecksum,
		path = :path,
		solvescript = :solvescript,
		templatable = :templatable,
		maxusers = :maxusers,
		category = :category,
		points = :points
	WHERE id = :id;`
)

// rebuildBuilds rebuilds the given builds of a challenge from its current
// source (the challenge row is expected to be persisted already): each is
// built, finalized, and its instances restarted or removed; the builder's
// copies are purged once the restarts are done; and, with pruneOldImages, the
// generation displaced from rollback retention is untagged once every build
// has been processed. A build whose rebuild fails is skipped: it keeps its
// previous generation on record (finalizeBuild is never reached) and stays
// reported as Stale until a later update rebuilds it. This is the whole of a
// rebuild; updateChallenges hands it the builds its selector picked
// (allBuildIds for a source change, staleBuildIds otherwise).
func (m *Manager) rebuildBuilds(metadata *ChallengeMetadata, buildIds []BuildId, pruneOldImages bool) []error {
	errs := []error{}
	if len(buildIds) == 0 {
		return errs
	}
	m.log.infof("rebuilding %d build(s) of %s", len(buildIds), metadata.Id)

	buildCtxFile, err := m.createBuildContext(metadata, m.GetDockerfile(metadata.ChallengeType))
	if err != nil {
		m.log.errorf("failed to create build context: %s", err)
		errs = append(errs, err)
		return errs
	}
	defer os.Remove(buildCtxFile)

	// Every build here belongs to the one challenge, so what the restarts
	// need of it is looked up once rather than per build: the persisted row
	// (`metadata` is the tree's copy that was just written; the round trip is
	// what every other launch path reads, and restarts are launches) and the
	// reverse port map.
	cMeta, err := m.lookupChallengeMetadata(metadata.Id)
	if err != nil {
		return append(errs, err)
	}
	revPortMap, err := m.getReversePortMap(metadata.Id)
	if err != nil {
		return append(errs, err)
	}

	replaced := []replacedImages{}
	for _, buildId := range buildIds {
		build, err := m.lookupBuildMetadata(buildId)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		// Resetting the flag signals to rebuild the Dockerfile
		build.Flag = ""
		err = m.executeBuild(metadata, build, buildCtxFile)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		candidate, reconcileErrs := m.reconcileBuild(build, cMeta, revPortMap, pruneOldImages)
		errs = append(errs, reconcileErrs...)
		if candidate != nil {
			replaced = append(replaced, *candidate)
		}
	}

	// Pruning waits until every build of this challenge has been
	// processed: other rows can still hold the displaced checksum
	// as their current or rollback generation until their own
	// rebuild lands (or if it failed above) — contentReferenced
	// keeps the images alive in all of those cases.
	m.pruneReplacedImages(replaced)
	return errs
}

// reconcileBuild commits a build's new generation and brings what runs it
// onto that generation. `build` carries the generation as executeBuild
// produced it -- or, on an external build plane, as a hand-over delivers
// it -- with the archive already in place, over a row that exists; the
// retention pair that generation displaces is read from the row here, so
// no caller captures it before the new identity is stamped. In order: the
// pair is rotated and the row finalized; on-demand instances are stopped
// and persistent ones restarted in place, or removed and relaunched; a
// persistent build is brought back to its count; the builder's copies are
// purged; and the generation that leaves retention is returned for the
// caller to prune once every build of the challenge is through (see
// pruneReplacedImages), or nil when nothing leaves.
func (m *Manager) reconcileBuild(build *BuildMetadata, cMeta *ChallengeMetadata, revPortMap map[string]string, pruneOldImages bool) (*replacedImages, []error) {
	// The pair the row holds until this generation lands: the checksum of
	// the generation it serves, which becomes the rollback target, and its
	// rollback target, which leaves retention. The row still answers for
	// the old generation because executeBuild stamps the new identity on
	// the build in memory only (see storedGeneration).
	oldChecksum, displaced, err := m.storedGeneration(build.Id)
	if err != nil {
		return nil, []error{err}
	}

	// Rotate the retention pair; a rebuild that reproduced the
	// same content checksum leaves the rollback target untouched.
	// The checksum covers every input to the build context (source,
	// flag format, base pins, the type's built-in Dockerfile -- see
	// contentChecksum), so a rebuild that changes the image changes
	// the checksum and rotates; what is left is the build's own
	// non-reproducibility (package installs), which the pins bound.
	build.PrevChecksum = rotatedPrevChecksum(oldChecksum, build.Checksum, displaced)

	// Update database
	if err := m.finalizeBuild(build); err != nil {
		return nil, []error{err}
	}

	// Stop and tear down existing instances. Persistent (schema-managed)
	// instances are restarted in place with the new image. On-demand
	// (dynamic) instances are started per user with injected env vars
	// that are not retained, so they cannot be restarted: they are
	// removed entirely (containers, network and record), and the
	// platform re-issues POST /builds/<id> for the ones it still wants,
	// instead of being left as hollow records that only a later stop or
	// the prune age would clear. Non-service challenges never run
	// instances, so any found here are placeholders left by older
	// versions of cmgr and are removed the same way.
	//
	// The lookup below purges on its way out. The purge is held
	// until after the restarts only to protect them, and this
	// path reaches no restart -- so there is nothing left to
	// wait behind, and skipping it would leak this build's
	// images for good: purgeBuiltImages is the only thing that
	// reclaims them.
	instances, err := m.getBuildInstances(build.Id)
	if err != nil {
		m.purgeBuiltImages(build)
		return nil, []error{err}
	}
	errs := []error{}
	for _, iid := range instances {
		instance, err := m.lookupInstanceMetadata(iid)
		if err != nil {
			// The list above is a moment old and the platform
			// stops on-demand instances continuously, so an id
			// that has gone between the two is ordinary rather
			// than a failure: for a dynamic build it is the
			// outcome this loop was about to produce anyway, and
			// for a persistent one the next converge relaunches
			// it. Reporting it would fail an operator's
			// update-schema for a race it cannot avoid.
			if _, gone := err.(*UnknownIdentifierError); gone {
				m.log.debugf("instance %d of %s went away before the rebuild reached it", iid, build.Challenge)
				continue
			}
			errs = append(errs, err)
			continue
		}
		// A row that has not finalized belongs to a launch still
		// running: the row is there, its containers are not yet.
		// Stopping it would delete the row under that launch —
		// ON DELETE CASCADE takes its container rows with it — so
		// the launch would fail on a foreign key, which cmgrd
		// reports as a plain 500 rather than the 503 the platform
		// retries. Leave it alone; it finishes on the generation
		// it read (see Start).
		if !instance.IsFinalized {
			continue
		}
		if build.InstanceCount == DYNAMIC_INSTANCES || !cMeta.NeedsInstance() {
			if err = m.stopInstance(instance); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		// Restart in place, or remove. The restart pulls the new generation
		// first, while the old one keeps serving, then swaps. One that cannot
		// happen (its worker down: every docker call to it would only time
		// out) or that fails at any point removes the instance instead, like
		// any stop on a down worker, and reports it; the converge below
		// relaunches it fresh, through placement. Left in place it would
		// either count as present while dead, or come back serving the old
		// image once its box rejoins, since a later update finds nothing to
		// rebuild.
		if instance.Worker != "" && m.workerIsDown(instance.Worker) {
			err = fmt.Errorf("worker %s is down", instance.Worker)
		} else {
			err = m.restartInstance(build, cMeta, instance, revPortMap)
		}
		if err != nil {
			// Two outcomes, told apart for the operator: removed, and
			// so relaunched by the converge below (its ports may differ,
			// which is why this stays an error of the update and not a
			// warning); or not even removable, in which case the record
			// survives, the converge counts it, and it has to be stopped
			// by hand.
			restartErr := err
			if err = m.stopInstance(instance); err != nil {
				err = fmt.Errorf("instance %d of %s could not be restarted (%v) nor removed (%v); it stays on record and must be stopped by hand before a converge can replace it", instance.Id, build.Challenge, restartErr, err)
			} else {
				err = fmt.Errorf("instance %d of %s removed instead of restarted (%v); relaunched through placement by this update, possibly on other ports", instance.Id, build.Challenge, restartErr)
			}
			m.log.warn(err)
			errs = append(errs, err)
		}
	}

	// A persistent build is brought back to its instance count here, by
	// the update that just rebuilt it, rather than by whoever next runs
	// update-schema: whatever the restarts above could not keep is
	// relaunched through placement (on another worker, if the one it
	// lived on is down). Dynamic and locked builds have no count to
	// converge to, and a non-service challenge runs no instances.
	if build.InstanceCount > 0 && cMeta.NeedsInstance() {
		errs = append(errs, m.convergeBuildInstances(build, cMeta, build.InstanceCount)...)
	}

	// The builder's copies are redundant once pushed
	// (purge.go), but only after the restarts above.
	// restartInstance goes through ensureImages, which pulls
	// for whichever daemon hosts the instance -- and a pull
	// that fails there does not merely cost time: the handler
	// above removes the instance instead of restarting it. So
	// purging first turns a registry blip in the middle of an
	// update into destroyed instances that stay down until
	// someone re-runs update-schema. Held until the instances
	// this build serves are running again, the cost of a blip
	// is back to what it was: nothing.
	m.purgeBuiltImages(build)

	// The displaced generation (two rebuilds back) leaves
	// retention; the just-replaced one survives as the
	// rollback target. Its tags are reconstructed over the
	// current host set — a generation built with different
	// hosts leaves strays for a future sweep to reclaim.
	if pruneOldImages && displacedPruneCandidate(oldChecksum, build.Checksum, displaced) {
		displacedMeta := BuildMetadata{
			Challenge: build.Challenge,
			Seed:      build.Seed,
			Format:    build.Format,
			Checksum:  displaced,
		}
		tags := make([]string, 0, len(build.Images))
		for _, image := range build.Images {
			tags = append(tags, m.instanceImageName(build.Challenge, &displacedMeta, image))
		}
		return &replacedImages{tags: tags, meta: displacedMeta}, errs
	}

	return nil, errs
}
