package record_test

import (
	"bytes"
	"fmt"
	"github.com/websines/structor-harness/record"
	"strings"
	"sync"
	"testing"
)

func TestAuditMalformedDigestDoesNotPanic(t *testing.T) {
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "0", strings.Repeat("0", 63), strings.Repeat("0", 65), strings.Repeat("g", 64), strings.Repeat("A", 64), "../outside", strings.Repeat("/", 64)} {
		t.Run(fmt.Sprintf("%q", suffix), func(t *testing.T) {
			digest := record.Digest("sha256:" + suffix)
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("malformed digest panicked: %v", p)
				}
			}()
			if _, err := cas.Get(digest); err == nil {
				t.Fatal("malformed digest accepted")
			}
			if cas.Has(digest) {
				t.Fatal("malformed digest reported present")
			}
		})
	}
}

func TestAuditConcurrentCASPutSameObject(t *testing.T) {
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("same object"), 100000)
	const n = 32
	start := make(chan struct{})
	results := make(chan error, n)
	clients := make([]*record.LocalCAS, n)
	for i := range n {
		clients[i], err = record.NewLocalCAS(cas.Root)
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := range n {
		writer := clients[i]
		wg.Go(func() {
			<-start
			d, err := writer.Put(content)
			if err == nil {
				_, err = cas.Get(d)
			}
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var failures []string
	for err := range results {
		if err != nil {
			failures = append(failures, fmt.Sprint(err))
		}
	}
	if len(failures) > 0 {
		t.Fatalf("concurrent idempotent puts failed %d/%d: %v", len(failures), n, failures)
	}
}
