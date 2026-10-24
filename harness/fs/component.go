package fs

import (
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// Component installs the filesystem capability rooted at root. It injects the
// tool runtime (to register read_file/write_file/edit_file) and provides the
// FileSystem service under hx.KeyFS. Both installations are revertible effects,
// so unloading the component withdraws the tools and the service in LIFO order.
func Component(root string) *runtime.Component {
	return &runtime.Component{
		Name:    "harness/fs",
		Inject:  []string{hx.KeyTools},
		Provide: []string{hx.KeyFS},
		Apply: func(ctx *runtime.Context) error {
			fs := &Local{Root: root}
			// Registering the three tools is an effect: RegisterTools hands back
			// the inverse that withdraws them, tracked on this fiber's stack.
			if err := ctx.Use(func() (func(), error) {
				v, err := ctx.Get(hx.KeyTools)
				if err != nil {
					return nil, err
				}
				tr, ok := v.(hx.ToolRuntime)
				if !ok {
					return nil, fmt.Errorf("fs: %q is not an hx.ToolRuntime", hx.KeyTools)
				}
				return RegisterTools(fs, tr)
			}); err != nil {
				return err
			}
			// Publish the capability last, so it appears only once its tools are in.
			return ctx.Provide(hx.KeyFS, fs)
		},
	}
}
