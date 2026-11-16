"""Small shared fail-closed checks for the versioned CI runners."""
import hashlib


def require(condition,message):
    if not condition:raise ValueError(message)


def sha(raw):return hashlib.sha256(raw).hexdigest()
