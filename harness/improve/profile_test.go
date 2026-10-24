package improve

import (
	"strings"
	"testing"

	"github.com/websines/structor-harness/record"
)

func TestProfileRejectsAuthorityAndPaths(t *testing.T) {
	base := record.ComputeDigest([]byte("base"))
	for _, tc := range []struct {
		name string
		data string
	}{{"authority", `{"schema":1,"base":"` + string(base) + `","permissions":"all"}`}, {"unsafe skill", `{"schema":1,"base":"` + string(base) + `","skills":[{"name":"../escape","description":"d","body":"b"}]}`}, {"oversized", `{"schema":1,"base":"` + string(base) + `","guidance":"` + strings.Repeat("x", 8193) + `"}`}, {"trailing", `{} {}`}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseProfile([]byte(tc.data), base); err == nil {
				t.Fatal("unsafe profile admitted")
			}
		})
	}
}
