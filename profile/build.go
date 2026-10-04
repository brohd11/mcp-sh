package profile

import (
	"regexp"

	"github.com/brohd11/mcp-sh/host"
	"github.com/brohd11/mcp-sh/host/mcphost"
	"github.com/brohd11/mcp-sh/host/scripts"
)

// Sources builds the config's command sources in precedence order: the native host,
// then upstream servers (by name), then script commands, which override both.
func (l *Loaded) Sources(version string) ([]host.Source, error) {
	p := l.Profile
	var sources []host.Source
	var bindings []scripts.Binding
	if h := p.Host; h != nil && !h.Disabled && h.Address != "" {
		client := &host.Client{Addr: h.Address, Token: h.Token, Hint: h.Hint}
		sources = append(sources, client)
		if h.Exec != nil {
			bindings = append(bindings, scripts.Binding{Host: client, Tool: h.Exec.Command, Lang: h.Exec.Lang})
		}
	}
	for _, name := range p.ServerNames() {
		s := p.MCPServers[name]
		cfg := mcphost.Config{
			Name: name, Type: s.Type,
			Command: s.Command, Args: s.Args, Env: s.Env,
			URL: s.URL, Headers: s.Headers,
			HideTools: s.HideTools, Defaults: s.Defaults,
			ImageDir: l.ImageDir,
			Version:  version,
		}
		if s.ErrorPattern != "" {
			cfg.ErrorPattern = regexp.MustCompile(s.ErrorPattern) // validated on load
		}
		srv := mcphost.New(cfg)
		sources = append(sources, srv)
		if e := s.Exec; e != nil {
			b := scripts.Binding{
				Server: srv, Tool: e.Tool, Param: e.Param, Args: e.Args, Lang: e.Lang,
				OutputPrefix: e.OutputPrefix, ErrorPrefix: e.ErrorPrefix,
			}
			if e.ErrorTrim != "" {
				b.ErrorTrim = regexp.MustCompile(e.ErrorTrim) // validated on load
			}
			// The filters were validated on load too.
			if e.OutputJq != "" {
				b.OutputJQ, _ = mcphost.CompileJQ(e.OutputJq)
			}
			if e.ErrorJq != "" {
				b.ErrorJQ, _ = mcphost.CompileJQ(e.ErrorJq)
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
