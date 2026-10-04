package profile

import (
	"regexp"

	"github.com/brohd11/agent-shell/host"
	"github.com/brohd11/agent-shell/host/mcphost"
	"github.com/brohd11/agent-shell/host/scripts"
)

// Sources builds the config's command sources in precedence order: the native host,
// then upstream servers (by name), then script commands, which override both.
func (l *Loaded) Sources(version string) ([]host.Source, error) {
	p := l.Profile
	var sources []host.Source
	if h := p.Host; h != nil && !h.Disabled && h.Address != "" {
		sources = append(sources, &host.Client{Addr: h.Address, Token: h.Token, Hint: h.Hint})
	}
	var bindings []scripts.Binding
	for _, name := range p.ServerNames() {
		s := p.MCPServers[name]
		cfg := mcphost.Config{
			Name: name, Type: s.Type,
			Command: s.Command, Args: s.Args, Env: s.Env,
			URL: s.URL, Headers: s.Headers,
			HideTools: s.HideTools, Defaults: s.Defaults,
			Version: version,
		}
		if s.ErrorPattern != "" {
			cfg.ErrorPattern = regexp.MustCompile(s.ErrorPattern) // validated on load
		}
		srv := mcphost.New(cfg)
		sources = append(sources, srv)
		if e := s.Exec; e != nil {
			b := scripts.Binding{
				Server: srv, Tool: e.Tool, Param: e.Param, Lang: e.Lang,
				OutputPrefix: e.OutputPrefix, ErrorPrefix: e.ErrorPrefix,
			}
			if e.ErrorTrim != "" {
				b.ErrorTrim = regexp.MustCompile(e.ErrorTrim) // validated on load
			}
			bindings = append(bindings, b)
		}
	}
	if len(bindings) > 0 {
		sources = append(sources, &scripts.Source{Dirs: l.CommandDirs, Bindings: bindings})
	}
	if len(sources) == 0 {
		return nil, ErrNoSources
	}
	return sources, nil
}
