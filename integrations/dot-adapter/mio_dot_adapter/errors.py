class AdapterError(Exception):
    """A safe, fixed error identifier; never put credentials or raw input here."""
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


class AuthenticationError(AdapterError):
    pass


class AuthorizationError(AdapterError):
    pass


class Unavailable(AdapterError):
    pass


class InvalidInput(AdapterError):
    pass


class StorageInvariantError(RuntimeError):
    """Must roll back. Never convert into success, cancellation, or automatic retry."""


class CallbackError(AdapterError):
    pass
