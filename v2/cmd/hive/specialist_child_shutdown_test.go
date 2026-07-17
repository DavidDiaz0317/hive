package main

import (
	"context"
	"testing"
	"time"
)

type recordingSpecialistChildShutdowner struct {
	calls       int
	deadline    time.Time
	hadDeadline bool
	events      *[]string
}

func (shutdowner *recordingSpecialistChildShutdowner) ShutdownSpecialistChildren(ctx context.Context) error {
	shutdowner.calls++
	shutdowner.deadline, shutdowner.hadDeadline = ctx.Deadline()
	if shutdowner.events != nil {
		*shutdowner.events = append(*shutdowner.events, "shutdown")
	}
	return nil
}

func TestOrdinaryMainShutdownUsesBoundedChildOnlyCleanup(t *testing.T) {
	shutdowner := &recordingSpecialistChildShutdowner{}
	started := time.Now()
	shutdownOrdinarySpecialistChildren(shutdowner, nil)
	if shutdowner.calls != 1 {
		t.Fatalf("specialist child shutdown calls = %d, want 1", shutdowner.calls)
	}
	if !shutdowner.hadDeadline {
		t.Fatal("specialist child shutdown did not receive a bounded context")
	}
	minimum := started.Add(ordinarySpecialistChildShutdownTimeout - time.Second)
	maximum := time.Now().Add(ordinarySpecialistChildShutdownTimeout + time.Second)
	if shutdowner.deadline.Before(minimum) || shutdowner.deadline.After(maximum) {
		t.Fatalf("specialist child shutdown deadline = %v, want a %s bound", shutdowner.deadline, ordinarySpecialistChildShutdownTimeout)
	}
}

func TestOrdinaryRuntimeReapsChildrenBeforeReleasingOwnership(t *testing.T) {
	events := []string{}
	shutdowner := &recordingSpecialistChildShutdowner{events: &events}
	applicationCtx, cancel := context.WithCancel(context.Background())
	shutdownOrdinaryVisualRuntime(cancel, shutdowner, func() {
		select {
		case <-applicationCtx.Done():
		default:
			t.Fatal("ownership released before application intake was canceled")
		}
		events = append(events, "release")
	}, nil)
	if len(events) != 2 || events[0] != "shutdown" || events[1] != "release" {
		t.Fatalf("ordinary runtime cleanup order = %v, want [shutdown release]", events)
	}
}
