# Setting this up on a Mac

Written for an Apple Silicon Mac. Two things need installing: Go, which is
required, and etcd, which is only needed for the live demo. Everything else is
in the repository — the project has no external dependencies, so there is
nothing to download and nothing that can break because a package moved.

Commands below are typed into **Terminal** (press Cmd+Space, type "Terminal").

## 1. Install Go

Check whether it is already there:

```
go version
```

If that prints a version of 1.21 or newer, skip ahead. If it says "command not
found", install it with Homebrew:

```
brew install go
```

No Homebrew? Install it first with the one-line command on https://brew.sh,
then run the line above. Alternatively download the macOS arm64 installer from
https://go.dev/dl and double-click it.

Confirm it worked:

```
go version
```

## 2. Run the tests

```
cd ~/Documents/adlmf
go test ./...
```

Expect `ok` against five packages. **Five tests will say SKIP** — those are the
etcd ones, and they skip themselves when no etcd is running. That is correct
behaviour, not a failure. Section 4 below turns them on.

To see every test by name:

```
go test ./... -v
```

To run them with Go's race detector, which is what the README quotes:

```
go test ./... -race
```

## 3. Run the experiments

This is the one that produces the numbers in `RESULTS.txt`:

```
go run ./cmd/experiments -trials 20
```

It takes roughly eight minutes and prints all ten hypotheses. Every run is
seeded, so the numbers come out identical each time — that is deliberate, so
results are reproducible rather than approximately repeatable.

For something quicker, run one hypothesis at a time (`h1` through `h10`):

```
go run ./cmd/experiments -only h10
go run ./cmd/experiments -only h7 -trials 5
```

To regenerate the results file itself:

```
go run ./cmd/experiments -trials 20 > RESULTS.txt
```

## 4. The live demo — real etcd, real processes, a real kill

This is the part worth showing a panel, because nothing about it is simulated.
Three separate programs compete for locks held in a real etcd, two of them
deliberately grabbing the same pair in opposite orders. The framework spots the
dangerous ordering, detects the deadlock, and kills one process to break it.
Then one of the processes is destroyed with `kill -9` while holding a lock, and
etcd's lease expiry hands that lock back without anything in our code noticing
the crash.

Install etcd:

```
brew install etcd
```

Start it in a Terminal window and **leave that window open**:

```
etcd
```

It will print a wall of startup log lines and then sit there. That is correct.

Open a **second** Terminal window and run:

```
cd ~/Documents/adlmf
go run ./cmd/e2e -for 12s
```

You will see the Pillar 1 warning appear, then deadlocks being detected and
broken, then the `kill -9`, and a summary ending in `PASS`. The line that
matters most is `mutual-exclusion violations : 0` — two services never held the
same lock at once, and that guarantee is enforced by etcd's consensus layer,
not by our own bookkeeping.

With etcd running, the five skipped tests now run too:

```
go test ./lockservice/ -v -run Etcd
```

When finished, stop etcd with Ctrl+C in its window.

## Fallback: prebuilt binaries, no Go needed

In `bin/` there are three ready-compiled programs for Apple Silicon Macs. They
exist as insurance — if Go will not install, or something breaks an hour before
a demo, these still run. Anything that needs recompiling obviously requires Go,
so treat these as a safety net rather than the normal route.

```
cd ~/Documents/adlmf/bin
./experiments -trials 5
```

The live demo needs the stub program pointed at explicitly, because normally it
compiles that itself:

```
./e2e -for 12s -stub ./stubservice
```

macOS may refuse to run them the first time, saying the developer cannot be
verified. If that happens, allow them once with:

```
xattr -d com.apple.quarantine ~/Documents/adlmf/bin/*
```

## If something goes wrong

**`go: command not found`** — Go is not installed, or the Terminal window was
opened before installing it. Close the window, open a new one, try again.

**`no such file or directory`** — you are not in the project folder. Run
`cd ~/Documents/adlmf` first.

**`SKIP: no etcd at http://127.0.0.1:2379`** — etcd is not running. Expected
unless you have done section 4.

**etcd says the port is in use** — an older etcd is still running. Find and
stop it with `pkill etcd`, then start it again.

**The experiments seem to hang** — they do not print until each hypothesis
finishes, and some take a minute or more. Use `-trials 5` to speed it up while
checking that it works at all.

**`could not compile the stub service`** — the demo tried to build a helper
program and Go was not available. Use the prebuilt one:
`./cmd/e2e -stub bin/stubservice`, or run it from `bin/` as above.

## What is where

```
README.md             what was built, what the measurements show, and what
                      does NOT hold up — read this before presenting
RESULTS.txt           raw output of all ten experiments
SETUP.md              this file

internal/types/       lock events
lockservice/          the lock layer: in-memory and real etcd
depmonitor/           deadlock detection, and the lock-order check (Pillar 1)
recovery/             the three baselines and our policy (Pillar 2)
faultmgr/             crash and partition detection
explain/              records why each decision was made
benchmark/            the simulator and the ten experiments
cmd/experiments/      runs the experiments
cmd/stubservice/      one service, as its own process, against real etcd
cmd/e2e/              the live demo described in section 4
bin/                  prebuilt Apple Silicon binaries (the fallback above)
```

A reasonable order to read the code in: `internal/types/events.go` first since
everything flows from those events, then `depmonitor/lockorder.go` for Pillar 1
and `recovery/policy.go` for Pillar 2. Those two files are the project's actual
contribution; the rest is scaffolding and measurement.
