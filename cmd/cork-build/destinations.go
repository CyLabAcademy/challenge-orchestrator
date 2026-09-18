package main

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DESTINATIONS_ENV names the file mapping destination names to the
// orchestrators they stand for.
const DESTINATIONS_ENV = "CORK_DESTINATIONS"

// destinations is that mapping: a short name, as a schema names it, to the
// address of the orchestrator that serves it.
//
// The indirection is the point. A schema says `destination: library`, not a
// URL, so a mistyped name is refused against a list this process holds
// rather than sending an event to whatever answers at a mistyped address --
// and moving an orchestrator is a change to this file rather than to every
// schema bound to it. The file is deployment topology and is written by the
// same ansible that stands the orchestrators up, which is also what puts it
// back on a rebuilt build plane: nothing here is derived from the database,
// so losing that database loses no routing.
type destinations struct {
	byName  map[string]string
	defName string // the only one, when there is only one
}

// loadDestinations reads the mapping. An absent file is not an error: a
// build plane handed --server directly needs none. One given neither is
// refused when it comes to route something (resolve), rather than building
// an event's worth of images and handing them nowhere.
func loadDestinations(path string) (*destinations, error) {
	d := &destinations{byName: map[string]string{}}
	if path == "" {
		return d, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return nil, fmt.Errorf("reading %s (%s): %w", DESTINATIONS_ENV, path, err)
	}
	// name -> address, and only that: an alias standing for two
	// orchestrators would put one schema's content on both, which is the
	// one thing the exclusivity rule exists to prevent.
	var raw map[string]string
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("reading %s (%s): %w", DESTINATIONS_ENV, path, err)
	}
	// Sorted, so a file with two faults names the same one every time.
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	// One address, one name. A second name for the same orchestrator reads
	// as a second destination everywhere else here: whereSchemaLives would
	// find every schema under both and report it served twice, which is the
	// state migrate-schema refuses to act on -- so nothing bound to it could
	// ever be moved again. Refused when the file is read, not when a
	// migration runs into it.
	byAddress := map[string]string{}
	for _, name := range names {
		if name == "" {
			return nil, fmt.Errorf("%s (%s) names a destination with no name", DESTINATIONS_ENV, path)
		}
		address := strings.TrimRight(raw[name], "/")
		if err := validAddress(address); err != nil {
			return nil, fmt.Errorf("destination '%s' in %s (%s): %w", name, DESTINATIONS_ENV, path, err)
		}
		if first, taken := byAddress[address]; taken {
			return nil, fmt.Errorf("destinations '%s' and '%s' in %s (%s) are both %s: one orchestrator is one destination, and a second name for it would make every schema there look as though two were serving it",
				first, name, DESTINATIONS_ENV, path, address)
		}
		byAddress[address] = name
		d.byName[name] = address
	}
	if len(d.byName) == 1 {
		for name := range d.byName {
			d.defName = name
		}
	}
	return d, nil
}

// validAddress is what serverList.Set asks of --server, asked of a
// configured destination for the same reason and at the same moment: when
// it is read, not when an event's worth of building has already been done.
func validAddress(value string) error {
	if value == "" {
		return fmt.Errorf("has no address")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("'%s' is not an address: %w", value, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("'%s' is not an http(s) address", value)
	}
	if parsed.Host == "" {
		return fmt.Errorf("'%s' names no host", value)
	}
	return nil
}

// resolve answers where a schema goes.
//
// A schema that names no destination is taken to mean the only one there is,
// and is refused once there is more than one. That is the whole rule, and it
// changes exactly when the risk appears: with one orchestrator configured
// there is nothing to be ambiguous about and no line to write, and the
// moment a second exists an unrouted schema is a real question -- an event's
// schema landing on the year-round orchestrator because a line was forgotten
// is precisely the accident worth refusing.
func (d *destinations) resolve(schemaName, destination string) (string, error) {
	if destination == "" {
		switch len(d.byName) {
		case 0:
			return "", fmt.Errorf("schema '%s' names no destination and none is configured: set %s, or give --server", schemaName, DESTINATIONS_ENV)
		case 1:
			return d.byName[d.defName], nil
		default:
			return "", fmt.Errorf("schema '%s' names no destination and there are %d to choose from (%s): say which one serves it",
				schemaName, len(d.byName), d.names())
		}
	}
	address, known := d.byName[destination]
	if !known {
		if len(d.byName) == 0 {
			return "", fmt.Errorf("schema '%s' is for destination '%s' and none is configured: set %s", schemaName, destination, DESTINATIONS_ENV)
		}
		return "", fmt.Errorf("schema '%s' is for destination '%s', which is not one of %s", schemaName, destination, d.names())
	}
	return address, nil
}

// names lists what is configured, for an error that has to be acted on.
func (d *destinations) names() string {
	names := make([]string, 0, len(d.byName))
	for name := range d.byName {
		names = append(names, "'"+name+"'")
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
