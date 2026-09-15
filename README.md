# Schedule

A task board for Windows. One executable, nothing else to install. Double-click
it and the board opens in its own window. Tasks are kept in a file of your
own, `schedule.json`, under your AppData folder.

## Where your tasks are kept

    %APPDATA%\Schedule\schedule.json          every task, your settings
    %APPDATA%\Schedule\backups\               one copy a day, the last 14 kept

**Settings › Data** has three ways to a backup:

- **Save backup…** writes the whole board — every task, comment and setting —
  to a file of your choosing, `schedule-backup-<date>.json`.
- **Restore…** takes such a file and replaces the board with it. What was
  there is kept first as `backups\schedule-before-restore-<time>.json`, so a
  restore can itself be undone by restoring that.
- **Show file** opens the folder above with `schedule.json` selected; a copy of
  it is a backup too. Each day, before the first change, the file is copied
  into `backups\` automatically.

Every change is written straight away, to a temporary file that is then
renamed over the real one, so a crash cannot leave a half-written file behind.
The file is plain JSON and readable in any editor.

To keep it somewhere else — a synced folder, say — point Schedule at it:

    schedule.exe -data "D:\OneDrive\schedule.json"

or set `SCHEDULE_DATA` to that path. Only one copy of Schedule should use a
given file at a time. If a second one does reach it anyway — the same synced
folder on two machines, say — nothing is silently lost. Schedule looks at the
file every few seconds and, when another program has changed it, re-reads it
and the board follows, with a line saying so. Should a write land in the same
moment as such a change, that one write is dropped rather than overwriting
the other side's work, and the message asks you to redo it.

The board follows changes made anywhere else, too: a second window, or another
machine on the same MongoDB. Within a few seconds of a change the page picks it
up and redraws, without touching what you are typing.

> **Upgrading from 1.7 or earlier?** Those versions kept tasks in MongoDB.
> The first time this version starts with no data file, it looks for a
> MongoDB on this machine and brings your tasks and settings across. After
> that MongoDB is not used, and you can uninstall it.

## Updates

Schedule keeps itself up to date. Shortly after it starts, and once a day
after that, it asks the update address for the newest version. A newer one is
downloaded and checked against its published hash, then installed without
anyone clicking anything:

- **at once** if the window is closed and Schedule is sitting in the tray,
  which is where it spends most of its time;
- otherwise **the moment you close the window**, so nothing is pulled out from
  under you, or **at the next start** if the window stays open all day.

Setup runs silently, closes the old copy, replaces it and starts the new one
where the old one was — in the tray, or on screen. Your tasks are not touched.
**Settings › About** shows the version and what is going on, and **Update
now** installs a downloaded update immediately instead of waiting. A
notification says when an update is ready and waiting for the window.

For this to work, `latest.json` and `Schedule-Setup.exe` have to be published
somewhere Schedule can fetch them over https — a GitHub release, a static
site, a shared folder served by a web server. `./build.sh installer` writes
`latest.json` next to the setup, ready to upload:

    {"version": "2.6", "file": "Schedule-Setup.exe", "sha256": "…", "notes": "What changed"}

`file` is resolved relative to where `latest.json` lives. `sha256` is the
setup's digest: the download is checked against it before it is run, and a
file that does not match is deleted and reported instead. Both addresses have
to be https; plain http is refused, except to this machine, for the tests.

The address is stamped into the exe at build time. The default is the GitHub
release:

    https://github.com/denniskramer-spec/schedule/releases/latest/download/latest.json

so publishing is `./build.sh installer` and then `./release.sh`, which pushes
the commit, tags it and creates the GitHub release with `Schedule-Setup.exe`
and `latest.json` attached, marked as the latest release. It needs the GitHub
CLI signed in, or `GITHUB_TOKEN` set. Another address goes in with `UPDATE_URL=… ./build.sh
installer` (or `make installer UPDATE_URL=…`); `SCHEDULE_UPDATE_URL` overrides
it per machine.

### MongoDB instead (optional)

If you want the board shared between machines, Schedule can still keep tasks
in a MongoDB server — local, on your network, or MongoDB Atlas:

    schedule.exe -mongo "mongodb://192.168.1.20:27017"
    schedule.exe -mongo "mongodb+srv://user:pass@cluster0.abc.mongodb.net/schedule"

or set `MONGODB_URI`. A database name in the URI is respected; `MONGODB_DB`
overrides it. Credentials in the URI are never printed or logged. If the
server cannot be reached, Schedule says so and does not start; when the
address is on this machine it also reports whether the MongoDB service is
missing, stopped or running.

## Tasks that run over several days

A task does not have to belong to one day. The composer has two date boxes: the
first day and the last. Leave them on the same day and you get the ordinary
task Schedule has always had. Set the last day later and the task runs over a
range — a one-day task is just a range one day wide, so there is no separate
kind of thing to keep track of.

A task that runs Monday to Friday shows on all five days, on the board, in the
week grid and as a dot in the month. Its card carries a badge saying which day
of the run you are looking at: `2/5` on Tuesday, `5/5` on Friday.

**The end-of-day prompt only asks for a verdict on the last day.** A task with
days still to run is not late, so Schedule does not ask whether it is done —
that question belongs on Friday, and asking it on Tuesday only teaches you to
dismiss the prompt.

**It does ask for a line every day, and that one is not optional.** On each day
of the run the prompt lists what is still going and wants a sentence about it.
The Close button stays disabled until every one is written. That is what stops
a week-long task arriving on Friday with nothing recorded for Monday through
Thursday — and it is what fills in the day-by-day history below.

You can still click away or press Escape to get out of it. Nothing is forced;
the prompt simply comes back, because the day is still missing its line.

Dragging a running task to another day in the week view moves the whole run and
keeps its length. Its badge turns red on any day that has gone by without a
comment.

## Carrying work forward

A task you did not finish does not stay behind on a day nobody looks at again.
When Schedule starts, and again when the clock rolls past midnight while it is
open, every unfinished task that has run out of days moves onto today.

Run out of days means the *last* day has passed, not the first — a task
planned for Monday to Friday is not late on Tuesday. When one does carry, only
its last day moves: the first day stays put, so the range still says which day
the work actually started, and the badge keeps counting.

The prompt comes first and the carry second. Schedule draws the board from what
is stored, so a task that ran out of days yesterday is still sitting on
yesterday when it is put to you. Carrying is the safety net for whatever you
leave unresolved, not a way of quietly skipping the question.

Nothing is lost in the move. Each task keeps a **history**: every comment you
have written on it, filed under the day you wrote it, and a line for each time
it rolled over. Click a task and you can read it day by day:

    Mon 8 Sep   Carried over from Fri 5 Sep
    Mon 8 Sep   Blocked on the API keys, chased ops        16:20
    Tue 9 Sep   Carried over from Mon 8 Sep
    Today       Keys arrived, starting this afternoon      09:40

The comment box in that sheet always starts empty, because it *adds* a day
rather than editing one. Old entries stay as they were written; the small
cross beside an entry removes it if it went in by mistake.

Cards that have rolled over show how many times, as `↻ 3`. That is the
signal that something has been slipping all week.

The close-out sheet is where you are asked, whether carrying is on or not. It
lists what has run out of days and offers **Done**, **Not done** or **Carry**,
each with a comment box, so whatever you type is filed under the day you are
closing. Carrying only decides what happens to what you leave behind: with it
on, that work lands on today; with it off, it stays where it is and you are
asked again next time.

Turn it off under **Settings › Carry unfinished tasks to the next day**.

## Repeating tasks

The composer's **Repeat** box — Once, Every day, Weekdays, Every week, Every
month — makes the task you add the first of a series. Schedule then plans its
later days ahead of time, about two months out, as tasks of their own: each
has its own column, its own comments and its own carries, and shows on the
board, the week and the month like anything else planned. Cards in a series
wear the rule as a badge. The planning happens when Schedule starts and when
the day turns, so the horizon keeps moving with you.

Every week means the same weekday as the first one; every month the same day
of the month, or the month's last day when it is shorter. A task that runs
over several days repeats with the same length.

The first task is where the series is managed. Open it and:

- **Change the rule**, or set it to **Once** to stop. The planned days nobody
  has touched — still to do, no comment — are dropped, and with a new rule
  they are planned afresh from today. Days already worked on stay as they are.
- **Rename it**, move it to another category or tick Agent, and the untouched
  planned days follow.
- **Delete it**, and the untouched planned days go with it; **Undo** puts
  everything back.

Opening one of the planned days shows a link back to the first one. Deleting
a planned day removes just that day, and it is not planned again. Each day
can be settled on its own: a daily task not done yesterday carries to today
next to today's, and the close-out sheet asks about both.

### Categories

The Day and Week boards are divided into rows by category — **Job**, **AI
training** and **Assets** to start with. Pick a task's category when you add
it, change it in the task sheet or the card's right-click menu, or drag the
card into another row.

Under **Settings › Categories** you can **rename** a row (its tasks stay put),
**add** one with **+ Add category** (up to 12), or **delete** one with **×**.
Deleting moves that row's tasks to the first row and shows **Undo**, which puts
the row and its tasks back. The last row cannot be deleted. Tasks from before
categories start in the first row.

### Working the board

- **Search** with the box in the top bar (<kbd>/</kbd> or <kbd>F</kbd> jumps
  to it). Every view narrows to tasks whose title, category or any comment
  contains all the words typed, in any order, and the heading says what is
  being matched. It only changes what is drawn: the close-out sheet still asks
  about every task. <kbd>Esc</kbd> clears it.
- **Save as .txt**, under the side panel, writes the day up as a report:

      9/10 Report

       -Bid 221 Done
       -Call status(1 final call done, 1 rescheduled)
      =================================================
      9/11  Todo

       -Continue 300 Bid

  Report lists what happened on the day: every task settled on it, done or
  not done, and every running task that got a comment that day, the comment
  in brackets. Todo is the next day: what is planned for it, plus whatever is
  still open and about to carry onto it. On the Week and Month views there is
  one Report block per day that has anything in it, and the Todo is for the
  day after the last one shown.
- **Right-click a card** for its column, its category, Open and Delete.
- **Keyboard:** <kbd>N</kbd> new task, <kbd>/</kbd> search, <kbd>T</kbd> today, <kbd>D</kbd> <kbd>W</kbd> <kbd>M</kbd>
  day, week and month, <kbd>←</kbd> <kbd>→</kbd> previous and next. On a card (Tab to
  it): <kbd>Enter</kbd> opens it, <kbd>1</kbd>–<kbd>4</kbd> set its column, <kbd>Delete</kbd>
  removes it.
- **Deleting** shows an **Undo** bar for a few seconds, which puts the task back
  with its whole history.
- **Category rows** fold away with a click on their name, on the Day and Week
  boards; a folded row still shows what is in it.
- The **close-out sheet** has a **Later** button. Leaving it any way — Later,
  Escape, clicking outside — keeps what you typed: comments on running tasks are
  saved, and comments on tasks you have not settled yet wait for next time. A
  task sheet with unsaved typing will not close on an outside click.
- **Dark mode** follows Windows (Settings › Personalisation › Colours).

### Start of day

**Settings › Start of day** picks where one day ends and the next begins,
00:00 to 23:00. **Today**, the day's lists and carrying over all switch at that
hour instead of midnight. Which way it moves depends on the hour:

- A **morning** hour lets the day run late. With 06:00, the 14th runs until
  06:00 on the 15th, so work at 02:00 still counts as the 14th.
- An **afternoon or evening** hour (12:00 or later) ends the day early. With
  23:00, the 14th runs from 23:00 on the 13th to 23:00 on the 14th, so 23:30
  already counts as the 15th.

### Deadline

Pick a time under **Settings › Deadline** (every half hour, or **Off**). Once
it has passed, a Windows notification says how many tasks are still open. Click it and the close-out sheet opens for today: **Done**, **Not done**
or **Carry** for each open task, with a comment box on each. If the window is
already open, the sheet opens there by itself, without pulling the window in
front of what you are doing.

While tasks are still open on that day, a reminder follows every 30 minutes,
up to three; closing the day out is what stops them.

It fires once per day, the first time Schedule sees the deadline behind it: at
the time itself, when Schedule starts or the computer wakes up after it, or
straight away if you set a time that has already gone today. The day it fired
is remembered, so restarting does not fire it again. On a day with nothing
open it stays quiet. The deadline belongs to the day as **Start of day** draws
it: with 23:00 and a 22:00 deadline, it is 22:00 on the same day.

> Upgrading from an earlier version? The first time the new build starts, each
> task's existing comment becomes the first entry in its history, and any task
> with no last day gets one equal to its first — which is what a one-day task
> has always meant, so nothing on your board changes. Carrying is only on by
> default for a fresh database: if you have used Schedule before, the setting
> keeps whatever it was, so tick it in Settings.

## Running it on Windows 11

1. Copy `schedule.exe` to the Windows machine — anywhere you like, the Desktop
   is fine. It needs no installer and writes nothing next to itself.
2. Double-click it.
3. Schedule opens in **its own window** — no tabs, no address bar, its own
   taskbar entry. There is no console window behind it.
4. Its icon sits in the notification area, beside the clock — under the **^**
   arrow until you drag it out onto the taskbar. Closing the window leaves
   Schedule running there — the first time, a notification says so:
   - **Click the icon** to open the window again.
   - **Right-click it** and choose **Quit** to stop Schedule.
   - The power button in the top right of the app also quits.

   Starting Schedule again from the Start menu while it is in the tray just
   brings the window back rather than starting a second copy.
5. Setup makes it **start with Windows**: at sign-in it waits in the tray
   without opening a window. Untick **Start with Windows** in the tray icon's menu to stop that;
   updating with Setup later keeps your choice.

### How the window works

Schedule renders in a Chromium window running in *app mode*, so it looks and
behaves like a desktop program rather than a web page. It looks for Chrome
first, then Edge, which ships with every Windows 11 install — so at least one
will be there.

That window gets its own browser profile, kept next to the database in
`%APPDATA%\Schedule\window`. It is separate from your normal browsing: your
tabs, history and logins are untouched, and Schedule never appears among them.
Deleting that folder is harmless; it is rebuilt on the next start.

If you would rather have it in an ordinary browser tab, start it with:

    schedule.exe -browser

Then it opens in whatever your default browser is. Clicking the tray icon
opens another tab, and you stop it with the power button in the app or
**Quit** on the tray icon.

### "Windows protected your PC"

You will almost certainly hit this the first time. The exe is not code-signed,
so SmartScreen blocks it by default. It is not a virus warning; it just means
Windows has not seen this file before.

Click **More info**, then **Run anyway**.

If you would rather clear the flag up front: right-click the file, choose
**Properties**, tick **Unblock** at the bottom of the General tab, and press OK.
That mark is added when a file arrives over a network or browser download, so
copying it in over a VMware shared folder or a USB stick often avoids it
entirely.

Windows Defender occasionally flags fresh, unsigned Go binaries as suspicious.
If it quarantines the file, that is a false positive — you can add an exclusion
for it, or build it yourself from this source.

### Which Windows

The build is x86-64 and targets Windows 7 or newer, so any Windows 11 machine
runs it. On an ARM-based Windows 11 device it runs under the built-in x64
emulation. To make a native ARM64 build instead:

    CGO_ENABLED=0 GOOS=windows GOARCH=arm64 \
      go build -trimpath -ldflags="-s -w -H windowsgui" -o schedule-arm64.exe .

### If it will not start

Startup failures show the reason in a message box. Anything else worth
knowing goes to `%APPDATA%\Schedule\schedule.log`. The previous run's log is
kept as `schedule.old.log`, so the reason for a crash survives restarting. The usual one is a second copy already running —
close the first window and try again.

Nothing needs a firewall exception. The server binds to `127.0.0.1`, which
Windows treats as local-only, so no "allow access" prompt should appear. If one
does, you can safely cancel it.

The server listens on `127.0.0.1` only, so nothing on your network can reach it.
If port 8765 is already in use it picks a free port instead and prints the
address it actually used; `schedule.log` records it in that case.

## What is in the data file

    { "version": 1,
      "prefs":    { ...settings... },
      "alertDay": "2026-09-14",        the day the deadline last fired
      "tasks":    [ ...one object per task... ] }

With `-mongo`, the same things live in the `schedule` database as
`schedule.tasks` (one document per task) and `schedule.prefs`.

A task holds `date` and `end`: the first and last day of its run, inclusive
and equal for a one-day task. It also carries a `log` array, which is its
history: one entry per comment or move, in the order they happened.

A repeating task's first day carries `repeat`, its rule, and
`repeatedThrough`, the last day it has been planned to; each planned day
carries `series`, the first day's id, and has an id of the form
`<first id>.<date>`.

    { kind: "note" | "carry" | "move",
      date:   "2026-09-09",           the day the entry belongs to
      at:     "2026-09-09 16:20",     when it was written
      text:   "...",                  the comment, if any
      from:   "2026-09-08",           carry and move: the day it left
      status: "doing" }               the task's column at the time

The log is capped at the most recent 500 entries per task.

Nothing is kept next to the exe. Everything is under `%APPDATA%\Schedule`:
the data file and its backups, `schedule.log`, and `window\`, the app window's
browser profile, which is disposable and rebuilt on the next start.

### Backing it up

Copy `schedule.json` — **Settings › Show file** takes you to it — or take one
of the daily copies from `backups\`. With `-mongo`, use MongoDB's own tools:
`mongodump --uri "mongodb://localhost:27017" --db schedule --out backup\`.

## Building from source

You need Go 1.22 or newer. Nothing else — no C compiler, no CGO. Every
dependency is pure Go, so it cross-compiles to Windows from Linux.

    ./build.sh run        # run it here on Linux to test
    ./build.sh windows    # produce schedule.exe
    ./build.sh installer  # produce Schedule-Setup.exe
    ./build.sh test       # go test ./...
    ./build.sh uitest     # drive the real app in a headless Chrome
    ./build.sh vet        # go vet ./...

A `Makefile` with the same targets is included if you have `make`.

### Tests

`test` runs the unit tests: validation, the start-of-day and deadline rules,
categories, and the data file (round trips, carrying over, backups).

`uitest` builds the program, starts it with no window on a free port against
a throwaway data file, and drives it in a headless Chrome the way a person
would: the close-out sheet, folding rows, the right-click menu, dragging,
keys, Undo, the task sheet, dark mode, adding and deleting categories, search,
repeating tasks, the board following a change made through another client
and one made to the file itself, a deadline set in the past and its
reminders, backup and restore, the update check against a local web server (a
setup with the wrong sha256 is refused), and what ends up in the file. It needs a
Chromium-based browser (set `CHROME` if it is not found) and takes about a
minute. `UITEST_SHOTS=some/folder` keeps screenshots.

The Windows build is just:

    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w -H windowsgui" -o schedule.exe .

`-H windowsgui` is what keeps the console window away. Startup errors appear
in a message box instead, and the log goes to `%APPDATA%\Schedule\schedule.log`.
To see output in a console while debugging, build without it.

`schedule.html` is baked into the binary from `static/`, so the `.exe` is the
only file you need to ship.

## Layout

    main.go          server, routes, app window, shutdown
    tray.go          notification-area icon, reopening the window
    deadline.go      the daily deadline and its reminders
    repeat.go        repeating tasks: the rules and the planning ahead
    refresh.go       the change number pages reload on
    backup.go        Save backup and Restore
    update.go        the update check, download and silent install
    icon.ico         the app icon: exe, tray, window
    rsrc_*.syso      icon.ico and version info, linked into the exe
                     (./build.sh icons regenerates them)
    console_*.go     Windows: message boxes, log file, window focus
    notify.go        Windows notifications: deadline, still-running hint
    icon.png         the icon for notifications and the Linux tray
    db.go            task and settings types, the Store interface, startup
    filestore.go     the JSON data file, backups
    mongo.go         the optional MongoDB store
    installer/       Schedule-Setup.exe
    schedule_test.go unit tests; filestore_test.go for the data file
    uitest/          browser tests against the real program
    static/
      schedule.html  the UI
    go.mod
    build.sh
    release.sh       publish the built installer as a GitHub release
    Makefile

## API

Everything is JSON over `127.0.0.1`. Requests carrying an `Origin` header from
anywhere other than this server are rejected.

    GET    /api/tasks        all tasks, ordered by date then created_at
    POST   /api/tasks        create one, returns the stored task
    PUT    /api/tasks/{id}   replace that task
    DELETE /api/tasks/{id}   delete one
    DELETE /api/tasks        delete every task (Settings > Clear data)

Every response carries `X-Rev`, a number that moves on with each change to
the board; `POST /api/alert`, which the page polls, returns it too, and the
page reloads when it has moved without the page's own doing. `POST /api/carry`
also plans repeating tasks ahead, and answers with `moved` and `added`.

    GET    /api/prefs        {"ask": true, "carry": true, "deadline": "17:30",
                              "dayStart": 6,
                              "categories": [{"id": 0, "name": "Job"},
                                             {"id": 1, "name": "AI training"}]}
    PUT    /api/prefs        same shape; deadline is "HH:MM" or "", dayStart
                             an hour from 0 to 23

    POST   /api/recategorize {"ids": [...], "category": 0} — file those tasks
                             under one category (deleting a row, and Undo)

    GET    /api/export       the whole board as a backup file
    POST   /api/import       replace the board with a backup file's contents

    GET    /api/update       version, and what the last check found
    POST   /api/update       check now
    POST   /api/update/install  install the downloaded Setup now (download first if need be)

    POST   /api/alert        the day a passed deadline wants closed out, or
                             {"day": ""}; answering clears it

    POST   /api/carry        roll work whose last day has passed onto today,
                             then return the board:
                             {"moved": 2, "tasks": [...]}. Does nothing unless
                             "carry" is on. Safe to call repeatedly.

A task posted or replaced must carry both `date` and `end`, and `end` may not
be before `date`. Its `category` is the `id` of the row it sits in.

    POST   /api/quit         shut the server down
    GET    /                 the UI

## Command line

    schedule.exe                      open in its own app window (default)
    schedule.exe -browser             open in your default browser instead
    schedule.exe -data "<path>"       keep tasks in this file instead
    schedule.exe -mongo "<uri>"       keep tasks in a MongoDB server instead
    schedule.exe -addr 127.0.0.1:0    listen on this address (tests use it)
    schedule.exe -noui                no window, tray or notifications

Environment variables:

    SCHEDULE_DATA  path of the data file, same as -data
    MONGODB_URI    connection string, same as -mongo
    MONGODB_DB     database name, overrides any name in the URI
