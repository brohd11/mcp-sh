package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// readRaw reads a config file as a top-level key map, so edits keep keys this version
// does not know about. A missing file is an empty map.
func readRaw(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return raw, nil
}

func writeRaw(path string, raw map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func editServers(path string, edit func(servers map[string]json.RawMessage) error) error {
	raw, err := readRaw(path)
	if err != nil {
		return err
	}
	servers := map[string]json.RawMessage{}
	if s, ok := raw["mcpServers"]; ok {
		if err := json.Unmarshal(s, &servers); err != nil {
			return fmt.Errorf("%s: mcpServers: %w", path, err)
		}
	}
	if err := edit(servers); err != nil {
		return err
	}
	if len(servers) == 0 {
		delete(raw, "mcpServers")
	} else {
		b, err := json.Marshal(servers)
		if err != nil {
			return err
		}
		raw["mcpServers"] = b
	}
	return writeRaw(path, raw)
}

// AddServer writes (or replaces) one mcpServers entry in the config file at path.
func AddServer(path, name string, s *Server) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid server name %q", name)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return editServers(path, func(servers map[string]json.RawMessage) error {
		servers[name] = b
		return nil
	})
}

// RemoveServer deletes an mcpServers entry from the file at path. Removing a server that
// comes from a lower layer (e.g. built-in) needs {"disabled": true} instead.
func RemoveServer(path, name string) error {
	return editServers(path, func(servers map[string]json.RawMessage) error {
		if _, ok := servers[name]; !ok {
			return fmt.Errorf("no server %q in %s", name, path)
		}
		delete(servers, name)
		return nil
	})
}

func editCommandDirs(path string, edit func(dirs []string) ([]string, error)) error {
	raw, err := readRaw(path)
	if err != nil {
		return err
	}
	var dirs []string
	if d, ok := raw["commandDirs"]; ok {
		if err := json.Unmarshal(d, &dirs); err != nil {
			return fmt.Errorf("%s: commandDirs: %w", path, err)
		}
	}
	if dirs, err = edit(dirs); err != nil {
		return err
	}
	if len(dirs) == 0 {
		delete(raw, "commandDirs")
	} else {
		b, err := json.Marshal(dirs)
		if err != nil {
			return err
		}
		raw["commandDirs"] = b
	}
	return writeRaw(path, raw)
}

// AddCommandDir appends dir to commandDirs in the config file at path.
func AddCommandDir(path, dir string) error {
	return editCommandDirs(path, func(dirs []string) ([]string, error) {
		if slices.Contains(dirs, dir) {
			return nil, fmt.Errorf("%s is already in %s", dir, path)
		}
		return append(dirs, dir), nil
	})
}

// RemoveCommandDir deletes dir from commandDirs in the config file at path.
func RemoveCommandDir(path, dir string) error {
	return editCommandDirs(path, func(dirs []string) ([]string, error) {
		i := slices.Index(dirs, dir)
		if i < 0 {
			if len(dirs) == 0 {
				return nil, fmt.Errorf("no commandDirs in %s", path)
			}
			return nil, fmt.Errorf("%s is not in %s (commandDirs: %s)", dir, path, strings.Join(dirs, ", "))
		}
		return slices.Delete(dirs, i, i+1), nil
	})
}

// EnsureUserConfig creates a minimal user config and commands folder for an app, so
// users have an obvious place to override it. It never overwrites an existing file.
// binary names the app's executable in the file's comment.
func EnsureUserConfig(app, binary string, o Options) (path string, created bool, err error) {
	path = UserConfigPath(app, o)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "commands"), 0o755); err != nil {
		return path, false, err
	}
	if fileExists(path) {
		return path, false, nil
	}
	raw := map[string]json.RawMessage{}
	comment, _ := json.Marshal(fmt.Sprintf("User overrides for %s. Keys here override the built-in config; mcpServers entries merge by name. See the merged result with: %s config show", binary, binary))
	raw["$comment"] = comment
	return path, true, writeRaw(path, raw)
}

// ImportSource is a config file that may define MCP servers to import.
type ImportSource struct {
	Label   string
	Servers map[string]*Server
}

// ImportSources reads mcpServers from Claude Code's configs: the project's .mcp.json,
// and ~/.claude.json (user-level servers and the project's local ones).
func ImportSources(projectDir, home string) []ImportSource {
	var out []ImportSource
	if s := readServers(filepath.Join(projectDir, ".mcp.json"), nil); len(s) > 0 {
		out = append(out, ImportSource{Label: filepath.Join(projectDir, ".mcp.json"), Servers: s})
	}
	claude := filepath.Join(home, ".claude.json")
	if s := readServers(claude, nil); len(s) > 0 {
		out = append(out, ImportSource{Label: claude + " (user)", Servers: s})
	}
	if key := projectKey(claude, projectDir); key != "" {
		if s := readServers(claude, []string{"projects", key}); len(s) > 0 {
			out = append(out, ImportSource{Label: claude + " (project " + projectDir + ")", Servers: s})
		}
	}
	return out
}

// projectKey finds projectDir among ~/.claude.json's "projects" keys. On Windows those
// can use forward slashes and another drive-letter case than the working directory.
func projectKey(claude, projectDir string) string {
	data, err := os.ReadFile(claude)
	if err != nil {
		return ""
	}
	var cfg struct {
		Projects map[string]json.RawMessage `json:"projects"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	if _, ok := cfg.Projects[projectDir]; ok {
		return projectDir
	}
	want := filepath.Clean(projectDir)
	for key := range cfg.Projects {
		got := filepath.Clean(filepath.FromSlash(key))
		if got == want || (runtime.GOOS == "windows" && strings.EqualFold(got, want)) {
			return key
		}
	}
	return ""
}

// readServers decodes the mcpServers map found under the given key path. Entries are
// decoded leniently: fields mcp-sh does not use are ignored.
func readServers(path string, keys []string) map[string]*Server {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var node any
	if json.Unmarshal(data, &node) != nil {
		return nil
	}
	for _, k := range keys {
		m, _ := node.(map[string]any)
		node = m[k]
	}
	m, _ := node.(map[string]any)
	entries, _ := m["mcpServers"].(map[string]any)
	out := map[string]*Server{}
	for name, e := range entries {
		b, _ := json.Marshal(e)
		var s Server
		if json.Unmarshal(b, &s) == nil {
			out[name] = &s
		}
	}
	return out
}

// FindImport looks up a server by name across sources, in order.
func FindImport(sources []ImportSource, name string) (*Server, string, error) {
	for _, src := range sources {
		if s, ok := src.Servers[name]; ok {
			return s, src.Label, nil
		}
	}
	var all []string
	for _, src := range sources {
		for n := range src.Servers {
			all = append(all, n)
		}
	}
	sort.Strings(all)
	if len(all) == 0 {
		return nil, "", fmt.Errorf("no MCP servers found in .mcp.json or ~/.claude.json")
	}
	return nil, "", fmt.Errorf("no server named %q (found: %v)", name, all)
}
