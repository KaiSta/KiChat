package runtime

import "testing"

// TestStoppingAFailedServerKeepsTheFailure: the readiness timeout reports the
// failure and then stops the process, and that stop used to overwrite the state
// with "stopped" — leaving a row whose Error explained a timeout while its state
// said a healthy server had been shut down. A load that timed out is not the same
// outcome as a server the user stopped.
func TestStoppingAFailedServerKeepsTheFailure(t *testing.T) {
	srv := NewServer("test")
	srv.fail("llama-server war nach 10s nicht bereit")
	srv.markStopped()

	st := srv.Status()
	if st.State != StateFailed {
		t.Errorf("state = %q, want %q", st.State, StateFailed)
	}
	if st.Error == "" {
		t.Error("the reason for the failure was cleared")
	}
	if st.PID != 0 || st.URL != "" {
		t.Errorf("a stopped server still reports pid %d url %q", st.PID, st.URL)
	}
}

// TestStoppingAHealthyServerReportsStopped is the other half of the guard: an
// ordinary stop must still report an ordinary stop.
func TestStoppingAHealthyServerReportsStopped(t *testing.T) {
	srv := NewServer("test")
	srv.setState(StateRunning)
	srv.markStopped()
	if st := srv.Status(); st.State != StateStopped {
		t.Errorf("state = %q, want %q", st.State, StateStopped)
	}
}
