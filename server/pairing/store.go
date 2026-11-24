package pairing

import (
	"crypto/rand"
	"math/big"
	"sync"
	"time"
)

// codeAlphabet leaves out look-alike characters (0/O, 1/l/I); 56 symbols.
const codeAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// CodeLength gives log2(56^20) ≈ 116 bits of entropy, so codes cannot be guessed even
// though fetching an installer needs no authentication.
const CodeLength = 20

// maxCodes bounds memory; deposits need an authenticated user, so this is only a safety net.
const maxCodes = 10000

type entry struct {
	deposit Deposit
	expires time.Time
}

// store keeps deposits in memory, keyed by pairing code. Codes do not survive a restart.
type store struct {
	mu      sync.Mutex
	entries map[string]entry
	now     func() time.Time
}

func newStore(now func() time.Time) *store {
	return &store{entries: make(map[string]entry), now: now}
}

func generateCode() (string, error) {
	n := big.NewInt(int64(len(codeAlphabet)))
	b := make([]byte, CodeLength)
	for i := range b {
		r, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		b[i] = codeAlphabet[r.Int64()]
	}
	return string(b), nil
}

// put stores a deposit under a new random code valid for ttl.
func (s *store) put(d Deposit, ttl time.Duration) (code string, expires time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	if len(s.entries) >= maxCodes {
		return "", time.Time{}, errTooManyCodes
	}
	for {
		code, err = generateCode()
		if err != nil {
			return "", time.Time{}, err
		}
		if _, taken := s.entries[code]; !taken {
			break
		}
	}
	expires = now.Add(ttl)
	s.entries[code] = entry{deposit: d, expires: expires}
	return code, expires, nil
}

// get returns the deposit of an unexpired code. With consume the code is removed,
// so a second fetch fails.
func (s *store) get(code string, consume bool) (Deposit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[code]
	if !ok {
		return Deposit{}, false
	}
	if !s.now().Before(e.expires) {
		delete(s.entries, code)
		return Deposit{}, false
	}
	if consume {
		delete(s.entries, code)
	}
	return e.deposit, true
}

func (s *store) purgeLocked(now time.Time) {
	for code, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, code)
		}
	}
}

func (s *store) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// failureLimiter counts failed code lookups per remote IP in a fixed window. It is
// separate from the API's login ban list, so devices sharing an IP with admins cannot
// lock the admins out of the API.
type failureLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	byIP   map[string]*failures
}

type failures struct {
	count int
	reset time.Time
}

func newFailureLimiter(maxFailures int, window time.Duration, now func() time.Time) *failureLimiter {
	return &failureLimiter{max: maxFailures, window: window, now: now, byIP: make(map[string]*failures)}
}

// blocked reports whether ip used up its failed lookups in the current window.
func (l *failureLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.byIP[ip]
	if !ok {
		return false
	}
	if !l.now().Before(f.reset) {
		delete(l.byIP, ip)
		return false
	}
	return f.count >= l.max
}

func (l *failureLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.byIP) > maxCodes {
		for k, f := range l.byIP {
			if !now.Before(f.reset) {
				delete(l.byIP, k)
			}
		}
	}
	f, ok := l.byIP[ip]
	if !ok || !now.Before(f.reset) {
		f = &failures{reset: now.Add(l.window)}
		l.byIP[ip] = f
	}
	f.count++
}
