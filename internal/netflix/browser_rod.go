package netflix

import (
	"netflix-household-validator/internal/models"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"netflix-household-validator/internal/logging"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

var activeRodSessions atomic.Int32

type RodBrowser struct{}

// NewRodBrowser creates a new instance of RodBrowser
func NewRodBrowser() *RodBrowser {
	return &RodBrowser{}
}

// OpenUpdatePrimaryLocation attempts to open the provided link using Rod, handling login if necessary.
func (rb *RodBrowser) OpenUpdatePrimaryLocation(link, netflixEmail, netflixPassword string, traceID string) (models.BrowserResult, error) {
	const maxAttempts = 3

	logging.Log.WithField("trace_id", traceID).Info("Open page with rod: ", link)

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		logging.Log.WithField("trace_id", traceID).Infof("Attempt %d/%d (fresh browser & profile)", attempt, maxAttempts)

		result, err := rb.attemptOpenLink(link, netflixEmail, netflixPassword, attempt, traceID)
		if err != nil {
			logging.Log.WithField("trace_id", traceID).WithError(err).Warnf("Attempt %d error", attempt)
		}

		switch result {
		case models.ResultSuccess, models.ResultExpired:
			return result, nil
		case models.ResultAbort:
			return result, nil
		case models.ResultFailed:
			if attempt < maxAttempts {
				backoff := time.Duration(attempt) * time.Second
				logging.Log.WithField("trace_id", traceID).Infof("Retrying in %s", backoff)
				time.Sleep(backoff)
			}
		}
	}

	logging.Log.WithField("trace_id", traceID).Warn("All attempts failed, giving up on link")
	return models.ResultFailed, nil
}

type pageOutcome int

const (
	outcomeUnknown   pageOutcome = iota
	outcomeLogin
	outcomeConfirmed
	outcomeExpired
)

// attemptOpenLink performs a single attempt to open the link and interact with the page.
func (rb *RodBrowser) attemptOpenLink(
	link string,
	netflixEmail string,
	netflixPassword string,
	attempt int,
	traceID string,
) (models.BrowserResult, error) {
	activeRodSessions.Add(1)
	defer activeRodSessions.Add(-1)

	locallog := logging.Log.WithField("trace_id", traceID)

	tmpDir, err := os.MkdirTemp("", "rod-netflix-*")
	if err != nil {
		locallog.WithError(err).Error("failed to create temp user data dir")
		return models.ResultFailed, err
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			locallog.WithError(err).Warn("failed to remove temp user data dir")
		}
	}()

	u := launcher.New().
		Headless(true).
		NoSandbox(true).
		UserDataDir(tmpDir)

	const systemChromium = "/usr/bin/chromium"
	if _, err := os.Stat(systemChromium); err == nil {
		u = u.Bin(systemChromium)
	}

	launchURL, err := u.Launch()
	if err != nil {
		locallog.WithError(err).Error("failed to launch browser")
		return models.ResultFailed, err
	}
	defer u.Cleanup()

	browser := rod.New()
	defer func() { _ = browser.Close() }()
	if err := browser.ControlURL(launchURL).Connect(); err != nil {
		locallog.WithError(err).Error("failed to connect to browser")
		return models.ResultFailed, err
	}

	page := browser.MustPage(link)
	defer func() { _ = page.Close() }()

	page.MustWaitLoad()

	// Accept cookie banner if present
	if cookieBtn, err := page.Timeout(3 * time.Second).Element("#onetrust-accept-btn-handler"); err == nil {
		locallog.Info("Cookie banner detected, accepting")
		cookieBtn.MustClick()
	}

	outcome, capturedLoginEl, err := racePageElements(page, 15*time.Second)
	if err != nil {
		locallog.WithError(err).Warnf("Attempt %d: page race failed", attempt)
		return models.ResultFailed, err
	}

	switch outcome {
	case outcomeConfirmed:
		locallog.Info("Clicked on confirm button successfully")
		return models.ResultSuccess, nil

	case outcomeExpired:
		locallog.Info("Expired link detected (upl-invalid-token present)")
		return models.ResultExpired, nil

	case outcomeLogin:
		if netflixEmail == "" || netflixPassword == "" {
			locallog.Info("Login required but credentials unavailable, aborting link")
			return models.ResultAbort, nil
		}
		locallog.Info("Login fields detected, attempting to log in")
		capturedLoginEl.MustInput(netflixEmail)
		page.MustElement(`input[name='password']`).MustInput(netflixPassword)
		page.MustElement(`[data-uia="login-submit-button"]`).MustClick()
		page.MustWaitLoad()

		// Cookie banner can reappear after login
		if cookieBtn, err := page.Timeout(3 * time.Second).Element("#onetrust-accept-btn-handler"); err == nil {
			locallog.Info("Cookie banner detected after login, accepting")
			cookieBtn.MustClick()
		}

		// After login, race between confirm button and expired token
		postOutcome, _, err := racePageElements(page, 15*time.Second)
		if err != nil {
			locallog.WithError(err).Warnf("Attempt %d: post-login page race failed", attempt)
			return models.ResultFailed, err
		}
		switch postOutcome {
		case outcomeConfirmed:
			locallog.Info("Clicked on confirm button successfully")
			return models.ResultSuccess, nil
		case outcomeExpired:
			locallog.Info("Expired link detected after login")
			return models.ResultExpired, nil
		default:
			locallog.Warnf("Attempt %d: unexpected state after login", attempt)
			return models.ResultFailed, nil
		}
	}

	locallog.Warnf("Attempt %d: timed out waiting for page elements", attempt)
	return models.ResultFailed, nil
}

// racePageElements races between login form, confirm button, and expired-token element.
// Returns the outcome and, for outcomeLogin, the captured login input element.
func racePageElements(page *rod.Page, timeout time.Duration) (pageOutcome, *rod.Element, error) {
	outcome := outcomeUnknown
	var capturedLoginEl *rod.Element

	_, err := page.Timeout(timeout).Race().
		Element(`input[name='userLoginId']`).Handle(func(e *rod.Element) error {
			outcome = outcomeLogin
			capturedLoginEl = e
			return nil
		}).
		Element(`[data-uia="set-primary-location-action"]`).Handle(func(e *rod.Element) error {
			if err := e.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return err
			}
			outcome = outcomeConfirmed
			return nil
		}).
		Element(`[data-uia="upl-invalid-token"]`).Handle(func(e *rod.Element) error {
			outcome = outcomeExpired
			return nil
		}).
		Do()

	return outcome, capturedLoginEl, err
}

// StartCleanup starts a background goroutine that cleans up old Rod temp directories
func StartCleanup() {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			if activeRodSessions.Load() > 0 {
				logging.Log.Info("Skipping /tmp cleanup: active Rod sessions detected")
				continue
			}

			pattern := filepath.Join(os.TempDir(), "rod-netflix-*")
			matches, err := filepath.Glob(pattern)
			if err != nil {
				logging.Log.WithError(err).Warn("Failed to glob temp directories")
				continue
			}

			for _, dir := range matches {
				if err := os.RemoveAll(dir); err != nil {
					logging.Log.WithError(err).Warnf("Failed to remove temp dir: %s", dir)
				} else {
					logging.Log.Infof("Cleaned up temp dir: %s", dir)
				}
			}
		}
	}()
}
