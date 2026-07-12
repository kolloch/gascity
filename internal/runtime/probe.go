package runtime

// ProviderCapabilities describes what a runtime provider can report.
// Not all providers support all wake-reason inputs.
type ProviderCapabilities struct {
	// CanReportAttachment is true if IsAttached returns meaningful results.
	CanReportAttachment bool
	// CanReportActivity is true if GetLastActivity returns meaningful results.
	CanReportActivity bool
}

// SessionSleepCapability describes how safely a runtime can participate in
// automatic idle sleep.
type SessionSleepCapability string

const (
	// SessionSleepCapabilityDisabled means idle sleep should be treated as off.
	SessionSleepCapabilityDisabled SessionSleepCapability = "disabled"
	// SessionSleepCapabilityTimedOnly means the runtime can participate in
	// timer-based sleep for headless sessions but cannot guarantee interactive
	// prompt-boundary safety.
	SessionSleepCapabilityTimedOnly SessionSleepCapability = "timed_only"
	// SessionSleepCapabilityFull means the runtime supports safe interactive
	// idle sleep, including attachment-aware grace windows.
	SessionSleepCapabilityFull SessionSleepCapability = "full"
)

// SleepCapabilityProvider is an optional extension for providers that can
// report idle sleep capability for the routed backend of a specific session.
type SleepCapabilityProvider interface {
	SleepCapability(name string) SessionSleepCapability
}
