package run

import (
	"context"
	"errors"
	"io"
	"testing"
)

type notifierFunc func(context.Context, string, map[string]any) error

func (f notifierFunc) Notify(ctx context.Context, ev string, p map[string]any) error {
	return f(ctx, ev, p)
}

func TestEngineFansOutToAllNotifiers(t *testing.T) {
	var a, b []string
	fa := notifierFunc(func(_ context.Context, ev string, _ map[string]any) error { a = append(a, ev); return nil })
	fb := notifierFunc(func(_ context.Context, ev string, _ map[string]any) error { b = append(b, ev); return errors.New("boom") })

	e := &Engine{out: io.Discard}
	WithNotifier(fa)(e)
	WithNotifier(fb)(e)

	e.notify(context.Background(), "fail", "m.yaml", "detail") // must not panic despite fb's error

	if len(a) != 1 || a[0] != "fail" || len(b) != 1 || b[0] != "fail" {
		t.Errorf("fan-out: a=%v b=%v, want both [fail]", a, b)
	}
}
