package cmgr

import (
	"fmt"
)

// reservePort claims a free host port for the instance on the daemon that
// will run it. Pools are per worker (” = local daemon): the same port number
// can be in use on every worker simultaneously.
func (m *Manager) reservePort(instance InstanceId, worker string, name string) (int, error) {
	if m.portLow == 0 {
		return 0, fmt.Errorf("port reservation disabled")
	}

	numPorts := m.portHigh - m.portLow + 1

	bitset, err := m.usedPortBitset(worker)
	if err != nil {
		return 0, err
	}

	for attempt := 0; attempt < 50; attempt++ {
		m.randMu.Lock()
		port := m.rand.Intn(numPorts) + m.portLow
		m.randMu.Unlock()
		var candidate int

		for i := 0; i < numPorts; i++ {
			p := port - m.portLow
			if (bitset[p/64] & (1 << (uint(p) % 64))) == 0 {
				candidate = port
				break
			}
			port = port + 1
			if port > m.portHigh {
				port = m.portLow
			}
		}

		if candidate == 0 {
			return 0, fmt.Errorf("All ports between %d and %d are in use", m.portLow, m.portHigh)
		}

		claimed, err := m.claimPort(instance, worker, name, candidate)
		if err != nil {
			return 0, err
		}
		if claimed {
			return candidate, nil
		}
		// Collision — mark this port as used in the local bitset and retry
		p := candidate - m.portLow
		bitset[p/64] |= 1 << (uint(p) % 64)
	}

	return 0, fmt.Errorf("failed to reserve a port after 50 attempts due to high contention")
}

// claimPort records the given host port for the instance on its worker if
// nobody else holds it there. The INSERT ... WHERE NOT EXISTS is the atomic
// check-and-claim reservePort relies on under concurrent launches.
func (m *Manager) claimPort(instance InstanceId, worker string, name string, port int) (bool, error) {
	query := `INSERT INTO portAssignments(instance, name, port, worker)
              SELECT ?, ?, ?, ?
              WHERE NOT EXISTS (SELECT 1 FROM portAssignments WHERE port = ? AND worker = ?);`
	res, err := m.db.Exec(query, instance, name, port, worker, port, worker)
	if err != nil {
		return false, err
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rowsAffected == 1, nil
}

// reassignPorts reserves the published ports of an instance that is being
// restarted in place (the rebuild path). stopContainers releases the previous
// assignments along with the container records, and startContainers takes
// the host port from instance.Ports; without a fresh reservation a restart
// with CMGR_PORTS set would hand docker port 0, land on an ephemeral port
// outside the range, and record no port at all (finalizeInstance persists
// read-back ports only when no range is configured). Each port first tries
// the number it had, so players keep the address they were given, and falls
// back to a new reservation if a concurrent launch took it. No-op without a
// port range: docker assigns the ports and the read-back records them.
func (m *Manager) reassignPorts(build *BuildMetadata, instance *InstanceMetadata, revPortMap map[string]string, previous map[string]int) error {
	if m.portLow == 0 {
		return nil
	}
	instance.Ports = make(map[string]int)
	for _, image := range build.Images {
		if image.Host == "builder" {
			continue
		}
		for _, portStr := range image.Ports {
			name := revPortMap[portStr]
			if prev, ok := previous[name]; ok && prev != 0 {
				claimed, err := m.claimPort(instance.Id, instance.Worker, name, prev)
				if err != nil {
					return err
				}
				if claimed {
					instance.Ports[name] = prev
					continue
				}
				m.log.warnf("port %d of instance %d was taken during its restart; assigning a new one", prev, instance.Id)
			}
			port, err := m.reservePort(instance.Id, instance.Worker, name)
			if err != nil {
				return err
			}
			instance.Ports[name] = port
		}
	}
	return nil
}

func (m *Manager) openInstance(meta *InstanceMetadata) error {
	res, err := m.db.NamedExec("INSERT INTO instances(build, lastsolved, worker) VALUES (:build, :lastsolved, :worker);", meta)

	if err != nil {
		m.log.errorf("failed to create instance entry: %s", err)
		return err
	}

	id, err := res.LastInsertId()
	if err != nil {
		m.log.errorf("failed to get instance id: %s", err)
		return err
	}

	meta.Id = InstanceId(id)
	return nil
}

func (m *Manager) finalizeInstance(meta *InstanceMetadata) error {
	txn := m.db.MustBegin()
	var err error
	if m.portLow == 0 {
		for name, port := range meta.Ports {
			_, err = txn.Exec("INSERT INTO portAssignments(instance, name, port, worker) VALUES (?, ?, ?, ?);",
				meta.Id,
				name,
				port,
				meta.Worker)

			if err != nil {
				m.log.errorf("failed to record port assignment: %s", err)
				cerr := txn.Rollback()
				if cerr != nil { // If rollback fails, we're in trouble.
					m.log.error(cerr)
					err = cerr
				}
				return err
			}
		}
	}

	for _, containerId := range meta.Containers {
		_, err = txn.Exec("INSERT INTO containers(instance, id) VALUES (?, ?);",
			meta.Id,
			containerId)

		if err != nil {
			m.log.errorf("failed to record containers: %s", err)
			cerr := txn.Rollback()
			if cerr != nil { // If rollback fails, we're in trouble.
				m.log.error(cerr)
				err = cerr
			}
			return err
		}
	}

	_, err = txn.Exec("UPDATE instances SET is_finalized = 1 WHERE id = ?;", meta.Id)
	if err != nil {
		m.log.errorf("failed to finalize instance: %s", err)
		cerr := txn.Rollback()
		if cerr != nil {
			m.log.error(cerr)
			err = cerr
		}
		return err
	}

	err = txn.Commit()
	if err != nil { // It's undocumented what this means...
		m.log.error(err)
	}
	return err
}

func (m *Manager) lookupInstanceMetadata(instance InstanceId) (*InstanceMetadata, error) {
	metadata := new(InstanceMetadata)
	txn := m.db.MustBegin()

	err := txn.Get(metadata, "SELECT * FROM instances WHERE id=?", instance)
	if isEmptyQueryError(err) {
		err = unknownInstanceIdError(instance)
	}

	ports := []struct {
		Name string
		Port int
	}{}
	if err == nil {
		err = txn.Select(&ports, "SELECT name, port FROM portAssignments WHERE instance=?", instance)
	}

	metadata.Ports = make(map[string]int)
	for _, kvPair := range ports {
		metadata.Ports[kvPair.Name] = kvPair.Port
	}

	metadata.Containers = []string{}
	if err == nil {
		err = txn.Select(&metadata.Containers, "SELECT id FROM containers WHERE instance=?", instance)
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

func (m *Manager) removeContainersMetadata(instance *InstanceMetadata) error {
	txn := m.db.MustBegin()
	_, err := txn.Exec("DELETE FROM portAssignments WHERE instance=?;", instance.Id)
	if err == nil {
		_, err = txn.Exec("DELETE FROM containers WHERE instance=?", instance.Id)
	}

	if err == nil {
		err = txn.Commit()
		if err != nil {
			m.log.errorf("failed to commit deletion of container metadata: %s", err)
		}
	} else {
		m.log.errorf("failed to delete container metadata: %s", err)
		closeErr := txn.Rollback()
		if closeErr != nil {
			m.log.errorf("rollback failed: %s", err)
			err = closeErr
		}
	}

	instance.Containers = []string{}
	instance.Ports = make(map[string]int)

	return err
}

func (m *Manager) removeInstanceMetadata(instance InstanceId) error {
	_, err := m.db.Exec("DELETE FROM instances WHERE id=?", instance)
	return err
}

const removedSchemaInstancesQuery = `
	SELECT instances.id
	FROM instances
	JOIN builds ON instances.build = builds.id
	WHERE builds.schema = ? AND instancecount = ?;`

func (m *Manager) removedSchemaInstances(schema string) ([]InstanceId, error) {
	instances := []InstanceId{}
	err := m.db.Select(&instances, removedSchemaInstancesQuery, schema, LOCKED)
	return instances, err
}

const buildInstancesQuery = `
	SELECT instances.id
	FROM instances
	WHERE build = ?;`

func (m *Manager) getBuildInstances(build BuildId) ([]InstanceId, error) {
	instances := []InstanceId{}
	err := m.db.Select(&instances, buildInstancesQuery, build)
	return instances, err
}

const recordInstanceSolveQuery = `
	UPDATE instances
	SET lastsolved = :lastsolved
	WHERE id = :id AND lastsolved < :lastsolved;`

const recordBuildSolveQuery = `
	UPDATE builds
	SET lastsolved = :lastsolved
	WHERE id = :build AND lastsolved < :lastsolved;`

// Records a solve directly against a build (no instance), used when checking
// builds that never get a running instance (artifact-only, flag-only). Reuses
// recordBuildSolveQuery; a single statement needs no transaction.
func (m *Manager) recordBuildSolve(build *BuildMetadata) error {
	_, err := m.db.NamedExec(recordBuildSolveQuery, map[string]interface{}{
		"build":      build.Id,
		"lastsolved": build.LastSolved,
	})
	if err != nil {
		m.log.errorf("failed to record build solve: %s", err)
	}
	return err
}

func (m *Manager) recordSolve(instance *InstanceMetadata) error {
	txn := m.db.MustBegin()
	_, err := txn.NamedExec(recordInstanceSolveQuery, instance)
	if err == nil {
		_, err = txn.NamedExec(recordBuildSolveQuery, instance)
	}

	if err == nil {
		err = txn.Commit()
		if err != nil {
			m.log.errorf("failed to commit deletion of container metadata: %s", err)
		}
	} else {
		m.log.errorf("failed to delete container metadata: %s", err)
		closeErr := txn.Rollback()
		if closeErr != nil {
			m.log.errorf("rollback failed: %s", err)
			err = closeErr
		}
	}
	return err
}

func (m *Manager) lookupBuildInstances(build BuildId) ([]*InstanceMetadata, error) {
	var instances []*InstanceMetadata
	txn, err := m.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer txn.Rollback() //nolint:errcheck

	err = txn.Select(&instances, "SELECT * FROM instances WHERE build = ?", build)

	// Fetch all ports for these instances
	ports := []struct {
		Instance InstanceId `db:"instance"`
		Name     string     `db:"name"`
		Port     int        `db:"port"`
	}{}
	if err == nil && len(instances) > 0 {
		err = txn.Select(&ports, "SELECT p.instance, p.name, p.port FROM portAssignments p JOIN instances i ON p.instance = i.id WHERE i.build = ?", build)
	}

	// Fetch all containers for these instances
	containers := []struct {
		Instance InstanceId `db:"instance"`
		Id       string     `db:"id"`
	}{}
	if err == nil && len(instances) > 0 {
		err = txn.Select(&containers, "SELECT c.instance, c.id FROM containers c JOIN instances i ON c.instance = i.id WHERE i.build = ?", build)
	}

	if err != nil {
		m.log.errorf("read of database failed: %s", err)
		return nil, err
	}

	if err = txn.Commit(); err != nil {
		m.log.errorf("failed to commit read-only transaction: %s", err)
		return nil, err
	}

	// Map ports to instances
	portMap := make(map[InstanceId]map[string]int)
	for _, p := range ports {
		if _, ok := portMap[p.Instance]; !ok {
			portMap[p.Instance] = make(map[string]int)
		}
		portMap[p.Instance][p.Name] = p.Port
	}

	// Map containers to instances
	containerMap := make(map[InstanceId][]string)
	for _, c := range containers {
		containerMap[c.Instance] = append(containerMap[c.Instance], c.Id)
	}

	// Combine
	for _, inst := range instances {
		inst.Ports = portMap[inst.Id]
		if inst.Ports == nil {
			inst.Ports = make(map[string]int)
		}
		inst.Containers = containerMap[inst.Id]
		if inst.Containers == nil {
			inst.Containers = []string{}
		}
	}

	return instances, nil
}
