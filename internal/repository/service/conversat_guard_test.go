package service

import (
	"sync"
	"testing"
)

func TestConversationGuardRejectsSameConversation(t *testing.T) {
	guard := NewConversationGuard()

	release, acquired := guard.TryAcquire(1)
	if !acquired {
		t.Fatal("first request should acquire the conversation")
	}
	defer release()

	secondRelease, secondAcquired := guard.TryAcquire(1)
	if secondAcquired {
		t.Fatal("second request must be rejected")
	}
	if secondRelease != nil {
		t.Fatal("rejected request must not receive a release function")
	}
}

func TestConversationGuardAllowsDifferentConversations(t *testing.T) {
	guard := NewConversationGuard()

	releaseA, acquiredA := guard.TryAcquire(1)
	if !acquiredA {
		t.Fatal("conversation 1 should be acquired")
	}
	defer releaseA()

	releaseB, acquiredB := guard.TryAcquire(2)
	if !acquiredB {
		t.Fatal("conversation 2 should be acquired independently")
	}
	defer releaseB()
}

func TestConversationGuardReusesAfterRelease(t *testing.T) {
	guard := NewConversationGuard()

	release, acquired := guard.TryAcquire(1)
	if !acquired {
		t.Fatal("conversation should be acquired")
	}

	release()

	secondRelease, secondAcquired := guard.TryAcquire(1)
	if !secondAcquired {
		t.Fatal("conversation should be reusable after release")
	}
	defer secondRelease()
}

func TestConversationGuardReleaseIsIdempotent(t *testing.T) {
	guard := NewConversationGuard()

	firstRelease, acquired := guard.TryAcquire(1)
	if !acquired {
		t.Fatal("first request should acquire the conversation")
	}

	firstRelease()

	secondRelease, acquired := guard.TryAcquire(1)
	if !acquired {
		t.Fatal("conversation should be acquired again")
	}

	// 第一次 release 的重复调用不能删除第二次占用。
	firstRelease()

	thirdRelease, acquired := guard.TryAcquire(1)
	if acquired {
		thirdRelease()
		t.Fatal("second request must still hold the conversation")
	}

	secondRelease()
}

func TestConversationGuardRejectsZeroID(t *testing.T) {
	guard := NewConversationGuard()

	release, acquired := guard.TryAcquire(0)
	if acquired {
		release()
		t.Fatal("zero conversation ID must be rejected")
	}
	if release != nil {
		t.Fatal("invalid conversation must not receive a release function")
	}
}

func TestConversationGuardConcurrentAcquire(t *testing.T) {
	guard := NewConversationGuard()

	const workers = 32

	var wg sync.WaitGroup
	var mu sync.Mutex
	acquiredCount := 0
	var release func()

	wg.Add(workers)

	for range workers {
		go func() {
			defer wg.Done()

			currentRelease, acquired := guard.TryAcquire(1)
			if !acquired {
				return
			}

			mu.Lock()
			acquiredCount++
			release = currentRelease
			mu.Unlock()
		}()
	}

	wg.Wait()

	if acquiredCount != 1 {
		t.Fatalf(
			"expected exactly one successful acquisition, got %d",
			acquiredCount,
		)
	}

	release()
}
