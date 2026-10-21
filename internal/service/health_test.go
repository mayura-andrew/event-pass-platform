package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeHealth struct {
	delay time.Duration
	err   error
}

func (f fakeHealth) EmailDelay(context.Context) (time.Duration, error) { return f.delay, f.err }

func TestHealthCheck(t *testing.T) {
	for _, c := range []struct {
		store        fakeHealth
		email        bool
		status, mail string
	}{
		{fakeHealth{}, true, "ok", "ok"},
		{fakeHealth{delay: 30 * time.Minute}, false, "ok", "off"},
		{fakeHealth{delay: 30 * time.Minute}, true, "degraded", "delayed 30 min"},
		{fakeHealth{err: errors.New("connection refused")}, true, "down", ""},
	} {
		rep := NewHealth(c.store, c.email).Check(context.Background())
		if rep.Status != c.status || rep.Checks["email"] != c.mail {
			t.Errorf("%+v: got %+v", c, rep)
		}
	}
}
