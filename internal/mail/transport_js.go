//go:build js

package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"syscall/js"
)

func defaultTransport() Transport { return jsTransport{} }

// jsTransport runs the SMTP dialogue through globalThis.goV2SMTP(requestJSON) -> Promise<resultJSON>,
// which the Worker edge binds to cloudflare:sockets. Must not be called from the JS event-loop goroutine.
type jsTransport struct{}

func (jsTransport) Send(ctx context.Context, cfg SMTPConfig, password, from string, to []string, msg []byte) error {
	fn := js.Global().Get("goV2SMTP")
	if fn.Type() != js.TypeFunction {
		return &Error{Kind: "smtp_unavailable"}
	}
	req, err := json.Marshal(map[string]any{
		"host": cfg.Host, "port": cfg.Port, "security": cfg.Security, "username": cfg.Username, "password": password,
		"from": from, "to": to, "message": base64.StdEncoding.EncodeToString(msg), "timeoutMs": cfg.TimeoutMs,
	})
	if err != nil {
		return &Error{Kind: "smtp_unavailable"}
	}
	done := make(chan struct{})
	var out string
	var failed bool
	ok := js.FuncOf(func(_ js.Value, args []js.Value) any {
		out = args[0].String()
		close(done)
		return nil
	})
	reject := js.FuncOf(func(_ js.Value, _ []js.Value) any {
		failed = true
		close(done)
		return nil
	})
	defer ok.Release()
	defer reject.Release()
	fn.Invoke(string(req)).Call("then", ok, reject)
	select {
	case <-done:
	case <-ctx.Done():
		// the callbacks stay referenced by the Promise; wait so Release is safe
		<-done
		return &Error{Kind: "smtp_timeout"}
	}
	var res struct {
		OK        bool   `json:"ok"`
		Kind      string `json:"kind"`
		Permanent bool   `json:"permanent"`
	}
	if failed || json.Unmarshal([]byte(out), &res) != nil {
		return &Error{Kind: "smtp_unavailable"}
	}
	if !res.OK {
		if res.Kind == "" || len(res.Kind) > 64 {
			res.Kind = "smtp_unavailable"
		}
		return &Error{Kind: res.Kind, Permanent: res.Permanent}
	}
	return nil
}
