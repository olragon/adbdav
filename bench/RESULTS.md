| Operation | Measured | Reference |
|---|---|---|
| **Listing a 76-entry directory** — on the device itself (`find` + `stat` over `adb`) | 0.142s | the floor |
| &nbsp;&nbsp;↳ PROPFIND `Depth: 1`, cold (fresh server, nothing cached) | 1.253s | |
| &nbsp;&nbsp;↳ PROPFIND `Depth: 1`, warm | 0.001s | |
| &nbsp;&nbsp;↳ PROPFIND `Depth: 1`, **`-cache 0`** — one `adb` round trip per entry | 18.070s | what a naive bridge costs |
| **Single `stat`** — PROPFIND `Depth: 0`, uncached | 0.387s | one `adb` round trip |
| **Read 77 MB** — HTTP GET through the mount | 6.600s (11.6 MB/s) | `adb exec-out cat`: 11.8 MB/s |
| &nbsp;&nbsp;↳ first 1 MiB only — ranged GET | 0.208s | |
| **Hash 77 MB on the device** — `sha256sum` for write verification | 0.177s | nothing transferred |

Device: 2511FPC34G. 5 runs each, median. Reads are of a 77 MB file.
Reproduce with `./bench/bench.sh`.
