package runtime

import (
	"reflect"
	"sync"
	"testing"
)

func TestReviewReplacementDisposesDependentBeforeProvider(t *testing.T) {
	r := New()
	var order []string
	r.Load(&Component{Name: "provider", Provide: []string{"service"}, Apply: func(ctx *Context) error {
		if err := ctx.Use(func() (func(), error) { return func() { order = append(order, "provider-v1") }, nil }); err != nil {
			return err
		}
		return ctx.Provide("service", "v1")
	}}, "")
	r.Load(&Component{Name: "consumer", Inject: []string{"service"}, Apply: func(ctx *Context) error {
		return ctx.Use(func() (func(), error) { return func() { order = append(order, "consumer") }, nil })
	}}, "")
	r.Load(&Component{Name: "provider", Provide: []string{"service"}, Apply: func(ctx *Context) error { return ctx.Provide("service", "v2") }}, "")
	if want := []string{"consumer", "provider-v1"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("replacement teardown order=%v, want dependent before provider %v", order, want)
	}
}

func TestReviewParentReplacementPreservesRemovalCascade(t *testing.T) {
	r := New()
	r.Load(&Component{Name: "parent"}, "")
	r.Load(&Component{Name: "child"}, "parent")
	r.Load(&Component{Name: "parent"}, "")
	r.Remove("parent")
	if child := r.Fiber("child"); child != nil {
		t.Fatalf("parent removed after replacement, but child remains registered in state %s", child.State())
	}
}

func TestReviewFiberStateDuringReload(t *testing.T) {
	r := New()
	provider := func() *Component {
		return &Component{Name: "provider", Provide: []string{"service"}, Apply: func(ctx *Context) error { return ctx.Provide("service", "value") }}
	}
	r.Load(provider(), "")
	f := r.Load(&Component{Name: "consumer", Inject: []string{"service"}}, "")
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range 1000 {
			r.Remove("provider")
			r.Load(provider(), "")
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 100000 {
			_ = f.State()
		}
	}()
	close(start)
	wg.Wait()
}
