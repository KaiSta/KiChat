package research

import (
	"kichat/internal/config"
	"net/http"
	"testing"
)

// blockedTargets are addresses the scraper must never reach with the default
// settings: the user's own machine, the local network, and the cloud metadata
// service. All of them are IP literals or a name handled without DNS, so this
// test stays offline.
var blockedTargets = []string{
	"http://127.0.0.1:1234/v1/models",
	"http://169.254.169.254/latest/meta-data/",
	"http://10.0.0.5/admin",
	"http://192.168.1.1/",
	"http://localhost:8080/",
	"http://[::1]/",
}

// redirectTo asks the scraper's own redirect policy what it thinks of following
// one more hop to target. via holds the requests already made; one entry keeps
// the chain under the limit, so an error can only come from the host guard.
func redirectTo(t *testing.T, s *Scraper, target string) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", target, err)
	}
	return s.client.CheckRedirect(req, []*http.Request{{}})
}

func TestScrapeRedirectHopIsGuarded(t *testing.T) {
	t.Run("refused by default", func(t *testing.T) {
		s := NewScraper(nil, ScrapeOptions{}, nil)
		for _, target := range blockedTargets {
			if err := redirectTo(t, s, target); err == nil {
				t.Errorf("redirect to %s was followed; the same URL is refused as an initial request", target)
			}
		}
	})

	t.Run("allowed when the user opts in", func(t *testing.T) {
		s := NewScraper(nil, ScrapeOptions{AllowPrivateHosts: true}, nil)
		for _, target := range blockedTargets {
			if err := redirectTo(t, s, target); err != nil {
				t.Errorf("redirect to %s blocked despite AllowPrivateHosts: %v", target, err)
			}
		}
	})

	t.Run("public addresses still follow", func(t *testing.T) {
		// An IP literal, so no DNS lookup is needed.
		s := NewScraper(nil, ScrapeOptions{}, nil)
		if err := redirectTo(t, s, "https://93.184.216.34/article"); err != nil {
			t.Errorf("public redirect blocked: %v", err)
		}
	})
}

func TestScrapeRedirectLimit(t *testing.T) {
	s := NewScraper(nil, ScrapeOptions{}, nil)
	req, err := http.NewRequest(http.MethodGet, "https://93.184.216.34/", nil)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, maxRedirects)
	if err := s.client.CheckRedirect(req, via); err == nil {
		t.Errorf("a chain of %d redirects was accepted", maxRedirects)
	}
}

// TestPrivateDialControl covers the check that runs on the address actually
// being dialled, which is what a hostname check cannot guarantee.
func TestPrivateDialControl(t *testing.T) {
	control := privateDialControl(false, config.LangEnglish)
	if control == nil {
		t.Fatal("privateDialControl(false) returned nil; the dial guard is disabled")
	}
	for _, address := range []string{"127.0.0.1:1234", "169.254.169.254:80", "10.1.2.3:443", "[::1]:80"} {
		if err := control("tcp", address, nil); err == nil {
			t.Errorf("dial to %s was allowed", address)
		}
	}
	if err := control("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("dial to a public address was blocked: %v", err)
	}
	if privateDialControl(true, config.LangEnglish) != nil {
		t.Error("privateDialControl(true) must not install a guard when the user allowed private hosts")
	}
}
