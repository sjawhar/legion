package main

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestProcAliveSeesAZombieAndAReapedProcessAsGone(t *testing.T) {
	if !procAlive(os.Getpid()) {
		t.Fatal("this test's own process reads as gone")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Until Wait reaps it, the exited child stays in /proc as a zombie.
	deadline := time.Now().Add(10 * time.Second)
	for procAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("exited, unreaped child %d still reads as alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Wait()
	if procAlive(pid) {
		t.Fatalf("reaped child %d reads as alive", pid)
	}
}

func TestSamplerRecordsAStackExitOnceAndFailsTheStep(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	gone := cmd.Process.Pid
	s := newSampler(endpoints{pids: map[string]int{"dispatch": gone, "postgres": os.Getpid()}})
	first := time.Date(2026, 10, 27, 9, 0, 15, 0, time.UTC)
	s.checkAlive(first)
	s.checkAlive(first.Add(5 * time.Second))
	exits := s.stackExits()
	if len(exits) != 1 || exits[0].Name != "dispatch" || exits[0].PID != gone || !exits[0].FirstSeenGone.Equal(first) {
		t.Fatalf("stack exits %+v, want dispatch once, first seen gone at %s", exits, first)
	}
	err := stackExitsError(exits)
	want := "dispatch (pid " + strconv.Itoa(gone) + ") exited during the step (first seen gone at 2026-10-27T09:00:15Z); this row does not measure the load"
	if err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
	if stackExitsError(nil) != nil {
		t.Fatal("a step with no stack exit fails")
	}
}

func TestSamplerChecksNothingWithoutPIDs(t *testing.T) {
	s := newSampler(endpoints{pids: map[string]int{}})
	s.checkAlive(time.Now())
	if exits := s.stackExits(); len(exits) != 0 {
		t.Fatalf("stack exits %+v under -allow-foreign, want none", exits)
	}
}
