package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/watch"
	kubev1 "kubevirt.io/api/core/v1"
)

type fakeWatch struct {
	ch chan watch.Event
}

func (f *fakeWatch) Stop()                  { close(f.ch) }
func (f *fakeWatch) ResultChan() <-chan watch.Event { return f.ch }

// 验证 debugListWatch 遵守 client-go 契约：ResultChan 每次返回同一 channel、
// 事件保序转发、Stop 后 channel 关闭（防止再次出现丢事件导致 VM 创建失败）。
func TestDebugListWatchContract(t *testing.T) {
	fw := &fakeWatch{ch: make(chan watch.Event, 8)}
	dlw := &debugListWatch{Interface: fw, resource: "virtualmachineinstances", selector: "test"}

	c1 := dlw.ResultChan()
	c2 := dlw.ResultChan()
	if c1 != c2 {
		t.Fatal("ResultChan() must return the same channel on every call")
	}

	go func() {
		fw.ch <- watch.Event{Type: watch.Added, Object: &kubev1.VirtualMachineInstance{}}
		fw.ch <- watch.Event{Type: watch.Modified, Object: &kubev1.VirtualMachineInstance{}}
		fw.ch <- watch.Event{Type: watch.Deleted, Object: &kubev1.VirtualMachineInstance{}}
		fw.Stop()
	}()

	var got []watch.EventType
	for ev := range c1 {
		got = append(got, ev.Type)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 events forwarded, got %d: %v", len(got), got)
	}
	want := []watch.EventType{watch.Added, watch.Modified, watch.Deleted}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event order mismatch at %d: got %v want %v", i, got, want)
		}
	}
}
