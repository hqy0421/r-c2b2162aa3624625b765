package report

import (
	"net/http"
)

// fakePage implements Page with programmable bridge responses. Each keyed
// script invocation is answered from a queue so tests can model a job that
// starts, stays pending, then completes.
type fakePage struct {
	pageURL   string
	title     string
	html      string
	scripts   []scriptResponse
	calls     []string // script bodies, in order
	cookies   []*http.Cookie
	urlErr    error
	htmlErr   error
	scriptErr error
	repeat    string // returned for every call once scripts is exhausted
}

type scriptResponse struct {
	value string
	err   error
}

func (p *fakePage) RunScript(body string, arguments map[string]interface{}, result interface{}) error {
	p.calls = append(p.calls, body)
	if p.scriptErr != nil {
		return p.scriptErr
	}
	if len(p.scripts) == 0 {
		if p.repeat != "" {
			if target, ok := result.(*string); ok {
				*target = p.repeat
			}
		}
		return nil
	}
	next := p.scripts[0]
	p.scripts = p.scripts[1:]
	if next.err != nil {
		return next.err
	}
	if target, ok := result.(*string); ok {
		*target = next.value
	}
	return nil
}

func (p *fakePage) URL() (string, error) {
	if p.urlErr != nil {
		return "", p.urlErr
	}
	if p.pageURL == "" {
		return "https://app.example.com/exports", nil
	}
	return p.pageURL, nil
}

func (p *fakePage) Title() (string, error) { return p.title, nil }

func (p *fakePage) HTML() (string, error) {
	if p.htmlErr != nil {
		return "", p.htmlErr
	}
	return p.html, nil
}

func (p *fakePage) GetCookies() ([]*http.Cookie, error) { return p.cookies, nil }

func (p *fakePage) ScriptCalls() []string { return p.calls }
