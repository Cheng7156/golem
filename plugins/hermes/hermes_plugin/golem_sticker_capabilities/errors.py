class CapabilityError(RuntimeError):
    """A safe-to-show capability error that never contains credentials."""


class RetryableCapabilityError(CapabilityError):
    """A transport or server failure that may succeed without changing the request."""
