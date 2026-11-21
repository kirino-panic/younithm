package providers

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLimiterSpacesCalls(t *testing.T) {
	now := time.Unix(1000, 0)
	var slept []time.Duration
	l := NewLimiter(time.Second)
	l.SetClock(func() time.Time { return now }, func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		now = now.Add(d)
		return nil
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := l.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != time.Second {
		t.Fatalf("slept = %v", slept)
	}
	l.Penalize(5 * time.Second)
	_ = l.Wait(ctx)
	if slept[len(slept)-1] != 5*time.Second {
		t.Fatalf("penalty not applied: %v", slept)
	}
}

func TestLimiterContextCancel(t *testing.T) {
	l := NewLimiter(time.Hour)
	_ = l.Wait(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("expected ctx error")
	}
}

type stubProvider struct{ calls int }

func (s *stubProvider) Name() string { return "stub" }
func (s *stubProvider) SearchTrack(context.Context, TrackQuery) ([]Track, error) {
	s.calls++
	return []Track{{Title: "x"}}, nil
}
func (s *stubProvider) GetTrack(context.Context, string) (*Track, error) {
	s.calls++
	return &Track{}, nil
}
func (s *stubProvider) GetArtwork(context.Context, string) ([]ImageRef, error) {
	s.calls++
	return nil, nil
}
func (s *stubProvider) GetArtistImage(context.Context, string) ([]ImageRef, error) {
	s.calls++
	return nil, nil
}

func TestWhenGatesEveryCall(t *testing.T) {
	stub := &stubProvider{}
	on := false
	g := When(stub, func(context.Context) bool { return on })
	if g.Name() != "stub" {
		t.Fatal("name")
	}
	ctx := context.Background()
	_, e1 := g.SearchTrack(ctx, TrackQuery{})
	_, e2 := g.GetTrack(ctx, "1")
	_, e3 := g.GetArtwork(ctx, "1")
	_, e4 := g.GetArtistImage(ctx, "1")
	for _, err := range []error{e1, e2, e3, e4} {
		if !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("inactive call returned %v", err)
		}
	}
	if stub.calls != 0 {
		t.Fatal("inactive wrapper reached the provider")
	}
	on = true
	if got, err := g.SearchTrack(ctx, TrackQuery{}); err != nil || len(got) != 1 || stub.calls != 1 {
		t.Fatalf("active: %v %v", got, err)
	}
}
