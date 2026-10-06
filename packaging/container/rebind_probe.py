"""DNS rebinding: resolve a name that points at a blocked address, then connect.

Resolution succeeding is fine and expected; a name-based egress rule would pass
this and still be bypassed. The pass condition is that the CONNECTION fails.
nip.io echoes the address encoded in the name, so this needs no DNS server under
our control.
"""
import socket

for host in ("169.254.169.254.nip.io", "127.0.0.1.nip.io", "10.0.0.1.nip.io"):
    try:
        addr = socket.gethostbyname(host)
    except Exception as e:
        print(f"  resolved  {host} -> FAILED ({type(e).__name__}) - no DNS egress either")
        continue
    print(f"  resolved  {host} -> {addr}  (resolution allowed)")
    s = socket.socket()
    s.settimeout(4)
    try:
        s.connect((addr, 80))
        print(f"  REACHABLE {host} -> {addr}  <-- REBINDING BYPASSED THE POLICY")
    except Exception as e:
        print(f"  blocked   {host} -> {addr}  ({type(e).__name__})")
    finally:
        s.close()
