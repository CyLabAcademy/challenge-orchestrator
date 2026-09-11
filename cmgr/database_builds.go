package cmgr

import (
	"fmt"
)

const openBuildQuery string = `
	INSERT INTO builds (
        flag,
        seed,
        format,
        checksum,
        hasartifacts,
        lastsolved,
        challenge,
        schema,
        instancecount
    )
    VALUES (
        :flag,
        :seed,
        :format,
        :checksum,
        :hasartifacts,
        :lastsolved,
        :challenge,
        :schema,
        :instancecount
    ) ON CONFLICT (schema, format, challenge, seed) DO
    UPDATE SET
    	instancecount = excluded.instancecount;`

func (m *Manager) openBuild(build *BuildMetadata) error {
	// Stamp the content checksum at creation so an in-flight build is already
	// visible to the reference checks that guard image untagging (see
	// contentReferenced): without it a new row sits at checksum=0 from creation
	// until finalizeBuild, and a concurrent destroy/prune in that window would
	// not count it as a reference and could remove images it resolves to. The
	// value matches what executeBuild stamps before building. Best-effort: if
	// the challenge's source checksum is unavailable the row keeps its incoming
	// value (finalizeBuild still stamps the real one on success).
	if build.Checksum == 0 {
		var challenge struct {
			SourceChecksum uint32 `db:"sourcechecksum"`
			ChallengeType  string `db:"challengetype"`
		}
		if err := m.db.Get(&challenge, "SELECT sourcechecksum, challengetype FROM challenges WHERE id = ?;", build.Challenge); err == nil {
			build.Checksum = m.buildContentChecksum(challenge.SourceChecksum, build.Format, challenge.ChallengeType)
		}
	}

	_, err := m.db.NamedExec(openBuildQuery, build)
	m.log.debugf("Opening %v", build)

	if err != nil {
		m.log.errorf("failed to open build (%s): %s", build.Challenge, err)
		return err
	}

	m.log.debug("Running select...")
	rows, err := m.db.NamedQuery("SELECT id, flag, hasartifacts, lastsolved, checksum, prevchecksum, sourcechecksum FROM builds WHERE schema=:schema AND format=:format AND challenge=:challenge AND seed=:seed;", build)
	if err != nil {
		m.log.errorf("failed to find build: %s", err)
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		err = fmt.Errorf("found no rows when exactly one expected for build of %s", build.Challenge)
		m.log.error(err)
		return err
	}
	// Returned rather than logged: a caller left with build.Id == 0 would go
	// on to build and finalize against a row that does not exist.
	err = rows.Scan(&build.Id, &build.Flag, &build.HasArtifacts, &build.LastSolved, &build.Checksum, &build.PrevChecksum, &build.SourceChecksum)
	if err != nil {
		m.log.errorf("failed to read build ID: %s", err)
		return err
	}
	if rows.Next() {
		m.log.error("found more rows than expected")
	}

	m.log.debugf("Build of %s has ID %d", build.Challenge, build.Id)
	return nil
}

const finalizeBuildQuery string = `
	UPDATE builds
	SET
		flag = :flag,
		hasartifacts = :hasartifacts,
		checksum = :checksum,
		prevchecksum = :prevchecksum,
		sourcechecksum = :sourcechecksum,
		lastsolved = 0
	WHERE id = :id;`

func (m *Manager) finalizeBuild(build *BuildMetadata) error {
	txn := m.db.MustBegin()
	res, err := txn.NamedExec(finalizeBuildQuery, build)

	if err != nil {
		m.log.errorf("failed to finalize build (%d): %s", build.Id, err)
		cerr := txn.Rollback()
		if cerr != nil { // If rollback fails, we're in trouble.
			m.log.error(cerr)
			err = cerr
		}
		return err
	}

	rowCount, err := res.RowsAffected()

	if err != nil {
		m.log.errorf("failed to check row count for build (%d): %s", build.Id, err)
		cerr := txn.Rollback()
		if cerr != nil { // If rollback fails, we're in trouble.
			m.log.error(cerr)
			err = cerr
		}
		return err
	}

	if rowCount != 1 {
		err = fmt.Errorf("finalized an unexpected number of builds: finalized %d expected 1", rowCount)
		m.log.error(err)
		cerr := txn.Rollback()
		if cerr != nil { // If rollback fails, we're in trouble.
			m.log.error(cerr)
			err = cerr
		}
		return err
	}

	_, err = txn.Exec("DELETE FROM lookupData WHERE build=?;", build.Id)
	if err != nil {
		m.log.errorf("failed to delete old lookups for build (%d): %s", build.Id, err)
		cerr := txn.Rollback()
		if cerr != nil { // If rollback fails, we're in trouble.
			m.log.error(cerr)
			err = cerr
		}
		return err
	}
	for k, v := range build.LookupData {
		_, err = txn.Exec("INSERT INTO lookupData(build, key, value) VALUES (?, ?, ?);",
			build.Id,
			k,
			v)

		if err != nil {
			m.log.errorf("failed to finalize lookups for build (%d): %s", build.Id, err)
			cerr := txn.Rollback()
			if cerr != nil { // If rollback fails, we're in trouble.
				m.log.error(cerr)
				err = cerr
			}
			return err
		}
	}

	_, err = txn.Exec("DELETE FROM images WHERE build=?;", build.Id)
	if err != nil {
		m.log.errorf("failed to delete old images for build (%d): %s", build.Id, err)
		cerr := txn.Rollback()
		if cerr != nil { // If rollback fails, we're in trouble.
			m.log.error(cerr)
			err = cerr
		}
		return err
	}
	for _, image := range build.Images {
		res, err := txn.Exec("INSERT INTO images(build, host) VALUES (?, ?);",
			build.Id,
			image.Host)
		if err != nil {
			m.log.errorf("failed to finalize images for build (%d/%s): %s", build.Id, image.Host, err)
			cerr := txn.Rollback()
			if cerr != nil { // If rollback fails, we're in trouble.
				m.log.error(cerr)
				err = cerr
			}
			return err
		}

		imageId, err := res.LastInsertId()
		if err != nil {
			m.log.error(err)
			cerr := txn.Rollback()
			if cerr != nil { // If rollback fails, we're in trouble.
				m.log.error(cerr)
				err = cerr
			}
			return err
		}

		image.Id = ImageId(imageId)

		for _, port := range image.Ports {
			_, err = txn.Exec("INSERT INTO imagePorts(image, port) VALUES (?, ?);",
				image.Id,
				port)

			if err != nil {
				m.log.errorf("failed to finalize ports for image (%d/%d): %s", build.Id, image.Id, err)
				cerr := txn.Rollback()
				if cerr != nil { // If rollback fails, we're in trouble.
					m.log.error(cerr)
					err = cerr
				}
				return err
			}
		}
	}

	err = txn.Commit()
	if err != nil { // It's undocumented what this means...
		m.log.error(err)
	}

	return err
}

// contentReferenced reports whether a build row other than excludeBuild
// resolves to the same docker tags as bMeta. A tag is exactly
// (challenge, seed, checksum, host) — see dockerId — so the key is challenge
// and seed with bMeta's checksum as either the row's current generation or its
// retained rollback generation (prevchecksum). Format is deliberately NOT in
// the key: it affects a tag only through the checksum (see contentChecksum),
// so two rows with different formats but an equal checksum genuinely share
// images and must count as references — matching on format too would miss
// them. Builds matching this way share images by construction, so callers must
// not untag images while this returns true. On a query error it fails safe
// (true): leaking an image tag is recoverable, deleting one still in use is not.
func (m *Manager) contentReferenced(bMeta *BuildMetadata, excludeBuild BuildId) bool {
	var count int
	err := m.db.Get(&count,
		"SELECT COUNT(*) FROM builds WHERE challenge=? AND seed=? AND (checksum=? OR prevchecksum=?) AND id != ?;",
		bMeta.Challenge, bMeta.Seed, bMeta.Checksum, bMeta.Checksum, excludeBuild)
	if err != nil {
		m.log.errorf("failed to check for builds sharing image tags: %s", err)
		return true
	}
	return count > 0
}

func (m *Manager) removeBuildMetadata(build BuildId) error {
	txn := m.db.MustBegin()
	_, err := txn.Exec("DELETE FROM images WHERE build=?", build)

	if err != nil {
		m.log.errorf("failed to delete images for build (%d): %s", build, err)
		cerr := txn.Rollback()
		if cerr != nil {
			m.log.errorf("rollback failed: %s", cerr)
			err = cerr
		}
		return err
	}

	_, err = txn.Exec("DELETE FROM builds WHERE id=?", build)

	if err != nil {
		m.log.errorf("failed to delete build (%d): %s", build, err)
		cerr := txn.Rollback()
		if cerr != nil {
			m.log.errorf("rollback failed: %s", cerr)
			err = cerr
		}
		return err
	}

	err = txn.Commit()
	if err != nil {
		m.log.errorf("failed to commit deletion of build: %s", err)
	}

	return err
}

func (m *Manager) lookupBuildMetadata(build BuildId) (*BuildMetadata, error) {
	metadata := new(BuildMetadata)
	txn := m.db.MustBegin()

	err := txn.Get(metadata, "SELECT * FROM builds WHERE id=?", build)
	if isEmptyQueryError(err) {
		err = unknownBuildIdError(build)
	}

	lookups := []struct {
		Key   string
		Value string
	}{}
	if err == nil {
		err = txn.Select(&lookups, "SELECT key, value FROM lookupData WHERE build=?", build)
	}

	metadata.LookupData = make(map[string]string)
	for _, kvPair := range lookups {
		metadata.LookupData[kvPair.Key] = kvPair.Value
	}

	metadata.Images = []Image{}
	if err == nil {
		err = txn.Select(&metadata.Images, "SELECT id, host FROM images WHERE build=?", build)
		if err == nil {
			for i, image := range metadata.Images {
				err = txn.Select(&metadata.Images[i].Ports, "SELECT port FROM imagePorts WHERE image=?", image.Id)
			}
		}
	}

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

func (m *Manager) schemaExists(schema string) (bool, error) {
	builds := []BuildId{}
	err := m.db.Select(&builds, "SELECT id FROM builds WHERE schema = ? LIMIT 1;", schema)
	return len(builds) > 0, err
}

func (m *Manager) removedSchemaBuilds(schema string) ([]BuildId, error) {
	builds := []BuildId{}
	err := m.db.Select(&builds, "SELECT id FROM builds WHERE schema = ? AND instancecount = ?;", schema, LOCKED)
	return builds, err
}

func (m *Manager) lockSchema(schema string) error {
	_, err := m.db.Exec("UPDATE builds SET instancecount = ? WHERE schema = ?;", LOCKED, schema)
	return err
}

func (m *Manager) getSchemaBuilds(schema string) ([]BuildId, error) {
	builds := []BuildId{}
	err := m.db.Select(&builds, "SELECT id FROM builds WHERE schema = ? ORDER BY challenge;", schema)
	return builds, err
}

func (m *Manager) queryForSchemas() ([]string, error) {
	schemas := []string{}
	err := m.db.Select(&schemas, "SELECT DISTINCT schema FROM builds;")
	return schemas, err
}

// staleBuildIds lists the builds of a challenge produced from a source
// generation other than the one the challenge row records: their last rebuild
// failed after updateChallenges had committed the new metadata, so they still
// serve an earlier generation. Unbuilt rows (no flag, no generation) are not
// stale — they have nothing to serve and are built by their schema's converge.
func (m *Manager) staleBuildIds(cMeta *ChallengeMetadata) ([]BuildId, error) {
	ids := []BuildId{}
	err := m.db.Select(&ids,
		"SELECT id FROM builds WHERE challenge=? AND flag != '' AND sourcechecksum != ? ORDER BY id;",
		cMeta.Id, cMeta.SourceChecksum)
	if err != nil {
		m.log.errorf("failed to look up stale builds of %s: %s", cMeta.Id, err)
		return nil, err
	}
	return ids, nil
}

// allBuildIds lists every build of a challenge: the rebuild selector for a
// source change, where every image is out of date.
func (m *Manager) allBuildIds(cMeta *ChallengeMetadata) ([]BuildId, error) {
	ids := []BuildId{}
	err := m.db.Select(&ids, "SELECT id FROM builds WHERE challenge=? ORDER BY id;", cMeta.Id)
	if err != nil {
		m.log.errorf("failed to look up the builds of %s: %s", cMeta.Id, err)
		return nil, err
	}
	return ids, nil
}

// staleChallengeSet names every challenge with a build staleBuildIds would
// list against the challenge's recorded source generation, in one query:
// DetectChanges consults it for every challenge whose source is unchanged --
// where the tree's generation is the recorded one, so the two predicates
// agree -- on every update, dry run and schema converge.
// challengeHasStaleBuild is staleChallengeSet's question for one challenge:
// a point lookup for the converge, which asks per challenge, where the
// update asks once for every challenge.
func (m *Manager) challengeHasStaleBuild(id ChallengeId) (bool, error) {
	var count int
	err := m.db.Get(&count, `SELECT COUNT(1) FROM builds AS b
		JOIN challenges AS c ON c.id = b.challenge
		WHERE b.challenge = ? AND b.flag != '' AND b.sourcechecksum != c.sourcechecksum;`, id)
	return count > 0, err
}

func (m *Manager) staleChallengeSet() (map[ChallengeId]bool, error) {
	ids := []ChallengeId{}
	err := m.db.Select(&ids, `SELECT DISTINCT b.challenge FROM builds AS b
		JOIN challenges AS c ON c.id = b.challenge
		WHERE b.flag != '' AND b.sourcechecksum != c.sourcechecksum;`)
	if err != nil {
		m.log.errorf("failed to look up challenges with stale builds: %s", err)
		return nil, err
	}
	stale := make(map[ChallengeId]bool, len(ids))
	for _, id := range ids {
		stale[id] = true
	}
	return stale, nil
}

// storedGeneration is the retention pair a build row holds: the content
// checksum of the generation it serves and that generation's rollback
// target. A row opened but never finalized serves no generation, whatever
// checksum openBuild stamped on it for the reference checks, and answers
// zeros: nothing to retain, nothing to displace.
func (m *Manager) storedGeneration(id BuildId) (checksum, prev uint32, err error) {
	var row struct {
		Flag         string `db:"flag"`
		Checksum     uint32 `db:"checksum"`
		PrevChecksum uint32 `db:"prevchecksum"`
	}
	if err := m.db.Get(&row, "SELECT flag, checksum, prevchecksum FROM builds WHERE id = ?;", id); err != nil {
		return 0, 0, err
	}
	if row.Flag == "" {
		return 0, 0, nil
	}
	return row.Checksum, row.PrevChecksum, nil
}
