package broker

import (
	"sync/atomic"
	"testing"
)

func BenchmarkChannelClose(b *testing.B) {
	b.ReportAllocs()
	var c chan bool
	for i := 0; i < b.N; i++ {
		c = make(chan bool)
		close(c)
	}
}

func BenchmarkIsWild(b *testing.B) {
	const topic = "system/user/1212121212121/private"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		isWildcard(topic)
	}
	b.SetBytes(int64(len(topic)))
}

func BenchmarkAtomicAdd(b *testing.B) {
	var a int32
	for i := 0; i < b.N; i++ {
		atomic.AddInt32(&a, 10)
	}
}
