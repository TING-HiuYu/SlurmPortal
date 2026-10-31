//go:build js && wasm

package d1

import (
	"context"
	"errors"
	"syscall/js"
)

// JSBridge calls globalThis.goD1Bridge(op, payload) which must return Promise<string>.
// Callers must not run on the JS event-loop goroutine, or awaiting the Promise deadlocks.
type JSBridge struct{}

// Call invokes the JavaScript goD1Bridge function and waits for its promise.
func (JSBridge) Call(ctx context.Context, op string, payload []byte) ([]byte, error) {
	fn := js.Global().Get("goD1Bridge")
	if fn.Type() != js.TypeFunction {
		return nil, errors.New("d1 bridge unavailable")
	}
	done := make(chan struct{})
	var (
		out []byte
		err error
	)
	ok := js.FuncOf(func(_ js.Value, args []js.Value) any {
		out = []byte(args[0].String())
		close(done)
		return nil
	})
	fail := js.FuncOf(func(_ js.Value, args []js.Value) any {
		err = errors.New(js.Global().Get("String").Invoke(args[0]).String())
		close(done)
		return nil
	})
	defer ok.Release()
	defer fail.Release()
	fn.Invoke(op, string(payload)).Call("then", ok, fail)
	select {
	case <-done:
		return out, err
	case <-ctx.Done():
		// the callbacks stay referenced by the Promise; wait so Release is safe
		<-done
		return nil, ctx.Err()
	}
}
