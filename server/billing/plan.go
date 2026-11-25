// Package billing is the plan model of hosted billing (billing and metering design, phases 2 and 3): plans read from a
// private plans file, each organization's plan and billing status, its transfer allowance and the limits that follow
// from it, and the frozen quantities of each closed billing period. It has no payment provider; a provider (Stripe)
// turns the closed periods into invoices. Everything here is off unless [billing] enabled (Postgres only).
package billing

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// ErrInvalid is wrapped by the errors of invalid plans files and inputs.
var ErrInvalid = errors.New("invalid billing input")

// Overage is what happens past a plan's transfer allowance.
type Overage string

const (
	// OverageBill bills the transfer past the allowance at the plan's price per GiB.
	OverageBill Overage = "bill"
	// OverageCap has no overage: the hard cap applies at the allowance.
	OverageCap Overage = "cap"
)

// GiB is a binary gigabyte, the unit of overage prices.
const GiB = int64(1) << 30

// Default caps, in percent of the allowance.
const (
	DefaultSoftCapPercent = 80
	DefaultHardCapPercent = 100
)

// ByteSize is a number of bytes, written in a plans file as "10GiB", "500MiB", "1TB" or a plain number of bytes.
type ByteSize int64

// RateLimit is a bandwidth in bytes per second, written in a plans file in bits per second: "2Mbit", "256kbit",
// "1Gbit" (decimal units) or "0" for no limit.
type RateLimit int64

// Plan is one plan of the plans file. Prices are in USD. Zero values mean "none": no base price, no devices
// included, no per-tunnel rate limit.
type Plan struct {
	ID   string `toml:"id" json:"id"`
	Name string `toml:"name" json:"name"`
	// BasePrice is the monthly price, including IncludedDevices.
	BasePrice float64 `toml:"base_price" json:"base_price"`
	// IncludedDevices are the billable devices the base price covers.
	IncludedDevices int64 `toml:"included_devices" json:"included_devices"`
	// PricePerDevice is the monthly price of each billable device above IncludedDevices. Zero on a plan without
	// a price per device: then the device limit (MaxClients) is what limits the fleet.
	PricePerDevice float64 `toml:"price_per_device" json:"price_per_device"`
	// MaxClients is the most registered devices (tenants.max_clients), a safety ceiling; nil is unlimited.
	MaxClients *int `toml:"max_clients" json:"max_clients"`
	// TransferBase and TransferPerDevice make the monthly transfer allowance: base + per device × billable devices.
	TransferBase      ByteSize `toml:"transfer_base" json:"transfer_base"`
	TransferPerDevice ByteSize `toml:"transfer_per_device" json:"transfer_per_device"`
	// Overage is "bill" (metered overage at OveragePricePerGiB) or "cap" (enforced at the hard cap).
	Overage            Overage `toml:"overage" json:"overage"`
	OveragePricePerGiB float64 `toml:"overage_price_per_gib" json:"overage_price_per_gib"`
	// SoftCapPercent of the allowance notifies the administrators (default 80).
	SoftCapPercent int `toml:"soft_cap" json:"soft_cap"`
	// HardCapPercent of the allowance throttles tunnels and refuses new ones and uploads: default 100 on "cap"
	// plans; on "bill" plans only an organization's spending limit sets a hard cap.
	HardCapPercent int `toml:"hard_cap" json:"hard_cap"`
	// TunnelRateLimit is the bandwidth of each tunnel connection in each direction; 0 is unlimited.
	TunnelRateLimit RateLimit `toml:"tunnel_rate_limit" json:"tunnel_rate_limit"`
	// ThrottleRate is the bandwidth of each tunnel connection past the hard cap (default DefaultThrottleRate).
	ThrottleRate RateLimit `toml:"throttle_rate" json:"throttle_rate"`
	// AnnualDiscountMonths is kept for annual plans (not offered yet).
	AnnualDiscountMonths int `toml:"annual_discount_months" json:"annual_discount_months,omitempty"`
	// ProviderPrices maps the parts of the price (base, devices, overage) to the payment provider's price ids.
	// rportd never creates prices.
	ProviderPrices map[string]string `toml:"provider_prices" json:"-"`
}

// DefaultThrottleRate is the per-tunnel rate past the hard cap when a plan sets none: 256 kbit/s, so SSH and a slow
// RDP session still work and bulk transfer doesn't.
const DefaultThrottleRate = RateLimit(256_000 / 8)

// Trial is the trial a new organization gets: TrialDays of Plan, capped while no payment method is on file, then
// the free plan.
type Trial struct {
	// Plan is the plan of the trial (default: the free plan, which makes the trial a no-op).
	Plan string `toml:"plan" json:"plan"`
	Days int    `toml:"days" json:"days"`
	// MaxClients and Transfer cap a trial without a payment method: the device limit and a capped allowance.
	MaxClients *int     `toml:"max_clients" json:"max_clients"`
	Transfer   ByteSize `toml:"transfer" json:"transfer"`
}

// Plans are the plans of a plans file.
type Plans struct {
	// Free is the id of the free plan: organizations without a billing record are on it, and trials without a
	// payment method fall back to it.
	Free  string `toml:"free_plan" json:"free_plan"`
	Trial Trial  `toml:"trial" json:"trial"`
	List  []Plan `toml:"plan" json:"plans"`

	byID map[string]*Plan
}

// Get returns the plan id, or nil.
func (p *Plans) Get(id string) *Plan {
	if p == nil {
		return nil
	}
	return p.byID[id]
}

// FreePlan returns the free plan.
func (p *Plans) FreePlan() *Plan { return p.Get(p.Free) }

// LoadPlans reads and validates the plans file path ([billing] plans_file).
func LoadPlans(path string) (*Plans, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("billing: plans file: %w", err)
	}
	p, err := ParsePlans(b)
	if err != nil {
		return nil, fmt.Errorf("billing: plans file %s: %w", path, err)
	}
	return p, nil
}

var planID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ParsePlans reads and validates a plans file: unknown keys, duplicate or malformed ids, negative prices or sizes,
// an unknown overage mode, caps out of range, a missing free plan and a trial of an unknown plan are refused.
func ParsePlans(b []byte) (*Plans, error) {
	p := &Plans{}
	dec := toml.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			return nil, fmt.Errorf("%w: %s", ErrInvalid, sm.String())
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Plans) validate() error {
	if len(p.List) == 0 {
		return fmt.Errorf("%w: no [[plan]]", ErrInvalid)
	}
	p.byID = map[string]*Plan{}
	for i := range p.List {
		pl := &p.List[i]
		if err := pl.validate(); err != nil {
			return err
		}
		if p.byID[pl.ID] != nil {
			return fmt.Errorf("%w: plan %q is defined twice", ErrInvalid, pl.ID)
		}
		p.byID[pl.ID] = pl
	}
	if p.Free == "" {
		p.Free = "free"
	}
	free := p.byID[p.Free]
	if free == nil {
		return fmt.Errorf("%w: free_plan %q is not a plan", ErrInvalid, p.Free)
	}
	if free.BasePrice != 0 || free.PricePerDevice != 0 || free.Overage != OverageCap {
		return fmt.Errorf("%w: the free plan %q must have no price and overage = \"cap\"", ErrInvalid, p.Free)
	}
	t := &p.Trial
	if t.Plan == "" {
		t.Plan = p.Free
	}
	if p.byID[t.Plan] == nil {
		return fmt.Errorf("%w: trial plan %q is not a plan", ErrInvalid, t.Plan)
	}
	if t.Days < 0 || t.Days > 366 {
		return fmt.Errorf("%w: trial days must be between 0 and 366", ErrInvalid)
	}
	if t.MaxClients != nil && *t.MaxClients < 0 {
		return fmt.Errorf("%w: trial max_clients must not be negative", ErrInvalid)
	}
	if t.Transfer < 0 {
		return fmt.Errorf("%w: trial transfer must not be negative", ErrInvalid)
	}
	return nil
}

func (pl *Plan) validate() error {
	if !planID.MatchString(pl.ID) {
		return fmt.Errorf("%w: plan id %q must be 1 to 63 lower-case letters, digits, '-' or '_'", ErrInvalid, pl.ID)
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: plan %q: "+format, append([]any{ErrInvalid, pl.ID}, a...)...)
	}
	if pl.Name == "" {
		pl.Name = pl.ID
	}
	for name, v := range map[string]float64{"base_price": pl.BasePrice, "price_per_device": pl.PricePerDevice, "overage_price_per_gib": pl.OveragePricePerGiB} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) || v > 1e7 {
			return bad("%s must be between 0 and 10000000", name)
		}
	}
	if pl.IncludedDevices < 0 {
		return bad("included_devices must not be negative")
	}
	if pl.MaxClients != nil && *pl.MaxClients < 0 {
		return bad("max_clients must not be negative")
	}
	if pl.TransferBase < 0 || pl.TransferPerDevice < 0 {
		return bad("transfer sizes must not be negative")
	}
	switch pl.Overage {
	case OverageBill:
		if pl.OveragePricePerGiB <= 0 {
			return bad(`overage = "bill" needs overage_price_per_gib`)
		}
	case OverageCap:
	case "":
		pl.Overage = OverageCap
	default:
		return bad(`overage must be "bill" or "cap", not %q`, pl.Overage)
	}
	if pl.SoftCapPercent == 0 {
		pl.SoftCapPercent = DefaultSoftCapPercent
	}
	if pl.SoftCapPercent < 1 || pl.SoftCapPercent > 100 {
		return bad("soft_cap must be between 1 and 100 (percent of the allowance)")
	}
	if pl.HardCapPercent == 0 && pl.Overage == OverageCap {
		pl.HardCapPercent = DefaultHardCapPercent
	}
	if pl.HardCapPercent != 0 && (pl.HardCapPercent < pl.SoftCapPercent || pl.HardCapPercent > 1000) {
		return bad("hard_cap must be between soft_cap and 1000 (percent of the allowance)")
	}
	if pl.TunnelRateLimit < 0 || pl.ThrottleRate < 0 {
		return bad("rates must not be negative")
	}
	if pl.ThrottleRate == 0 {
		pl.ThrottleRate = DefaultThrottleRate
	}
	if pl.TunnelRateLimit > 0 && pl.ThrottleRate > pl.TunnelRateLimit {
		pl.ThrottleRate = pl.TunnelRateLimit
	}
	return nil
}

// Allowance is the plan's transfer allowance for a whole period with devices billable devices.
func (pl *Plan) Allowance(devices int64) int64 {
	return int64(pl.TransferBase) + int64(pl.TransferPerDevice)*max(devices, 0)
}

// Cents converts USD to whole cents, rounded half away from zero.
func Cents(usd float64) int64 { return int64(math.Round(usd * 100)) }

var sizeRe = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)\s*$`)

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
}

// ParseByteSize reads "10GiB", "500MiB", "1TB", "0" or a number of bytes.
func ParseByteSize(s string) (ByteSize, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%w: size %q: want a number and a unit (B, KiB, MiB, GiB, TiB, KB, MB, GB, TB)", ErrInvalid, s)
	}
	unit, ok := sizeUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("%w: size %q: unknown unit %q", ErrInvalid, s, m[2])
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	v := f * unit
	if v > math.MaxInt64/2 {
		return 0, fmt.Errorf("%w: size %q is too large", ErrInvalid, s)
	}
	return ByteSize(math.Round(v)), nil
}

// UnmarshalText reads a size string (TOML strings).
func (b *ByteSize) UnmarshalText(text []byte) error {
	v, err := ParseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// String formats the size in the largest binary unit it is a whole number of.
func (b ByteSize) String() string {
	for _, u := range []struct {
		name string
		n    int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if b != 0 && int64(b)%u.n == 0 {
			return strconv.FormatInt(int64(b)/u.n, 10) + u.name
		}
	}
	return strconv.FormatInt(int64(b), 10) + "B"
}

var rateUnits = map[string]float64{"bit": 1, "kbit": 1e3, "mbit": 1e6, "gbit": 1e9}

// ParseRateLimit reads "2Mbit", "256kbit", "1Gbit" or "0" (bits per second) into bytes per second.
func ParseRateLimit(s string) (RateLimit, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%w: rate %q: want a number and a unit (bit, kbit, Mbit, Gbit)", ErrInvalid, s)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	unit := strings.ToLower(m[2])
	if unit == "" && f == 0 {
		return 0, nil
	}
	mult, ok := rateUnits[unit]
	if !ok {
		return 0, fmt.Errorf("%w: rate %q: unknown unit %q (bit, kbit, Mbit, Gbit per second)", ErrInvalid, s, m[2])
	}
	v := f * mult / 8
	if v > 1e12 {
		return 0, fmt.Errorf("%w: rate %q is too large", ErrInvalid, s)
	}
	if v > 0 && v < 1 {
		v = 1
	}
	return RateLimit(math.Round(v)), nil
}

// UnmarshalText reads a rate string (TOML strings).
func (r *RateLimit) UnmarshalText(text []byte) error {
	v, err := ParseRateLimit(string(text))
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// String formats the rate in bits per second.
func (r RateLimit) String() string {
	bits := int64(r) * 8
	switch {
	case bits == 0:
		return "0"
	case bits%1_000_000_000 == 0:
		return strconv.FormatInt(bits/1_000_000_000, 10) + "Gbit"
	case bits%1_000_000 == 0:
		return strconv.FormatInt(bits/1_000_000, 10) + "Mbit"
	case bits%1000 == 0:
		return strconv.FormatInt(bits/1000, 10) + "kbit"
	}
	return strconv.FormatInt(bits, 10) + "bit"
}

// MonthStart is the start of the UTC calendar month containing t: billing periods are calendar months in UTC.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// DayStart is the start of the UTC day containing t.
func DayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
