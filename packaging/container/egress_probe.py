"""Egress probe: attempts CONNECTIONS and reports whether they succeeded.

Lives here rather than inline in verify-egress.sh. An earlier version embedded it
in a heredoc piped through `docker exec`, which never reached the process: every
probe was silent, and silence read exactly like a clean pass.
"""
import socket
import sys


def connect(label, host, port, family):
    s = socket.socket(family, socket.SOCK_STREAM)
    s.settimeout(4)
    try:
        s.connect((host, port))
        print(f"REACHABLE {label} ({host}:{port})")
    except Exception as e:
        print(f"blocked   {label} ({host}:{port}) - {type(e).__name__}")
    finally:
        s.close()


# Each argument is label|host|port|family.
for spec in sys.argv[1:]:
    label, host, port, fam = spec.split("|")
    connect(label, host, int(port), getattr(socket, fam))
