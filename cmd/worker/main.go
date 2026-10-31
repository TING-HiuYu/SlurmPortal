//go:build js && wasm

// worker is the Cloudflare Worker entry point. JavaScript only bridges bindings (D1, mTLS metadata,
// cron); every business decision happens here, in the same code the native server runs.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"syscall/js"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	_ "github.com/TING-HiuYu/SlurmPortal/internal/plugins/all"
	"github.com/TING-HiuYu/SlurmPortal/internal/store/d1"
)

const maxBody = 1 << 20

var instance *app.App

func main() {
	env := js.Global().Get("goV2Env")
	getenv := func(k string) string {
		v := env.Get(k)
		if v.Type() != js.TypeString {
			return ""
		}
		return v.String()
	}
	cfg, err := config.FromEnv(getenv)
	if err == nil {
		instance, err = app.New(context.Background(), cfg, d1.New(d1.JSBridge{}))
	}
	ready := js.Global().Get("goV2Ready")
	if err != nil {
		ready.Invoke(err.Error())
		return
	}
	js.Global().Set("goV2Fetch", promised(func(args []js.Value) any { return serve(args[0]) }))
	js.Global().Set("goV2Tick", promised(func([]js.Value) any { return errString(instance.Tick(context.Background())) }))
	js.Global().Set("goV2Publish", promised(func([]js.Value) any { return errString(instance.Publish(context.Background())) }))
	js.Global().Set("goV2Flush", promised(func([]js.Value) any { return errString(instance.Flush(context.Background())) }))
	js.Global().Set("goV2TakeWrites", js.FuncOf(func(js.Value, []js.Value) any { return instance.TakeWrites() }))
	ready.Invoke("")
	select {}
}

func errString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

// promised runs f on a goroutine: blocking a JS callback while D1 promises are pending would deadlock.
func promised(f func([]js.Value) any) js.Func {
	return js.FuncOf(func(_ js.Value, args []js.Value) any {
		executor := js.FuncOf(func(_ js.Value, p []js.Value) any {
			resolve := p[0]
			go func() { resolve.Invoke(f(args)) }()
			return nil
		})
		defer executor.Release()
		return js.Global().Get("Promise").New(executor)
	})
}

func result(status int, h http.Header, body []byte) js.Value {
	arr := js.Global().Get("Uint8Array").New(len(body))
	js.CopyBytesToJS(arr, body)
	pairs := []any{}
	for k, vs := range h {
		for _, v := range vs {
			pairs = append(pairs, []any{k, v})
		}
	}
	return js.ValueOf(map[string]any{"status": status, "headers": pairs, "body": arr})
}

// serve converts the private bridge envelope into an http.Request. "peer" is set only by the JS
// edge after it verified the client certificate; it is never read from HTTP headers.
func serve(in js.Value) js.Value {
	jsBody := in.Get("body")
	if jsBody.Length() > maxBody {
		return result(http.StatusRequestEntityTooLarge, nil, nil)
	}
	body := make([]byte, jsBody.Length())
	js.CopyBytesToGo(body, jsBody)
	req, err := http.NewRequest(in.Get("method").String(), in.Get("url").String(), bytes.NewReader(body))
	if err != nil {
		return result(http.StatusBadRequest, nil, nil)
	}
	headers := in.Get("headers")
	for i := 0; i < headers.Length(); i++ {
		pair := headers.Index(i)
		req.Header.Add(pair.Index(0).String(), pair.Index(1).String())
	}
	if req.URL.Scheme == "https" {
		req.TLS = &tls.ConnectionState{}
	}
	req.RemoteAddr = in.Get("clientIp").String() + ":0"
	if peer := in.Get("peer"); peer.Type() == js.TypeString && peer.String() != "" {
		req = req.WithContext(authn.WithBridgePeer(req.Context(), peer.String()))
	}
	rec := httptest.NewRecorder()
	instance.Handler.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return result(resp.StatusCode, resp.Header, out)
}
