"""OmniVM SDK -- thin in-VM helper for agents running inside OmniVM microVMs."""

from omnivm.workspace import workspace_path, ensure_dirs
from omnivm.secrets import get_secret, require_secret
from omnivm import log

__version__ = "0.1.0"
