# ax (agentswitch) tmux bindings.
# Source this from tmux.conf:   run-shell '~/path/to/agentswitch/tmux/ax.tmux'
# or copy the bindings below directly into your config.
#
# These add (prefix shown as whatever chord you use):
#   <prefix> a   resume a past session (Ctrl-N inside starts a new one)
#   <prefix> A   start a new session (pick harness, pick or create a directory)
#
# Fullscreen popup gives the choose-tree takeover feel; drop to -w 85% -h 80%
# for a floating modal box instead. <prefix> c is left alone (new-window).

# -s 'bg=terminal' makes the popup use the real terminal background (e.g. an
# OSC-set theme color) instead of tmux's plain popup fill, which renders black.
bind-key a display-popup -E -B -s 'bg=terminal' -w 100% -h 100% "ax pick"
bind-key A display-popup -E -B -s 'bg=terminal' -w 100% -h 100% "ax new"

# Optional: show a live session count in the status bar (polls every 5s via
# status-interval). Uncomment and fold into your existing status-right.
# set -ag status-right '#(ax list | wc -l | tr -d " ") sessions '
