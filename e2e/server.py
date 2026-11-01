#!/usr/bin/env python3
"""Stand-in game server for e2e/gameserver-fake.sh.

Behaves like the parts of a real dedicated server the driver has to get right:
listens on a UDP port and answers queries, logs to stdout, records its pid, and
on SIGINT/SIGTERM writes a "save" file before exiting 0 (a server that is
killed before it can save is the failure that matters in production).

Usage: server.py [port]   (default 27099). Files go in the current directory,
which the driver sets to install_dir.
"""
import os
import signal
import socket
import sys

port = int(sys.argv[1]) if len(sys.argv) > 1 else 27099
game_dir = os.getcwd()


def save_and_exit(signum, _frame):
    with open(os.path.join(game_dir, "save.txt"), "w") as f:
        f.write("saved on %s\n" % signal.Signals(signum).name)
    print("saved on %s, exiting" % signal.Signals(signum).name, flush=True)
    sys.exit(0)


signal.signal(signal.SIGINT, save_and_exit)
signal.signal(signal.SIGTERM, save_and_exit)

with open(os.path.join(game_dir, "server.pid"), "w") as f:
    f.write(str(os.getpid()))

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("127.0.0.1", port))
print("server listening on udp/%d pid=%d" % (port, os.getpid()), flush=True)

while True:
    data, addr = sock.recvfrom(1024)
    sock.sendto(b"PONG " + data, addr)
