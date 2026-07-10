# Recipe: Parallel design pipeline with human-gated publish

ax coordinates a design-to-draft pipeline: one worker per niche fans out in
parallel, each producing a design image, marketplace listing copy, and an
unpublished product draft. The coordinator then pauses at `ax ask`: nothing
publishes until a human approves.

This recipe automates the mechanical fan-out, copy generation, and draft creation
so a human can focus on the judgment calls: which niches to enter, which designs
to approve, which drafts to publish. It does not guarantee any commercial outcome.

---

## Prerequisites

You bring two thin adapters:

| Adapter | What it is | How to wire it |
|---|---|---|
| Image API | Any image-generation API with a REST interface | Set `IMAGE_API_KEY` and replace the body of `scripts/image-api.sh` with a real curl call |
| Print-on-demand backend | Any POD service with a REST API | Set `PRINTIFY_API_TOKEN` and `PRINTIFY_SHOP_ID` and replace the body of `scripts/create-draft.sh` with real curl calls |

Every adapter in `scripts/` ships as a dry-run mock: it writes artifacts to
disk, calls no API, prints a `MOCK:` banner to stderr, and spends nothing. The
filenames are the production names, so going live means replacing a script's
body, not renaming anything. The coordinator behavior, the worker task, and the
accept check stay identical.

---

## Folder layout

```
pod-dryrun/
  recipe.md                    this file
  behaviors/
    pod.md                     coordinator behavior
  scripts/
    check-drop.sh              accept check (used in dry run and production)
    image-api.sh               mock: placeholder PNG (real body: your image API)
    listing-copy.sh            mock: canned copy (real body: an LLM worker)
    create-draft.sh            mock: upload + create draft.json (real body: POD API)
    publish.sh                 human-gated: refuses until a human approves
  starter/
    niches.json                seed niche list (3 slots, swap in your own)
  runs/
    merch-drop-dryrun/
      drops/                   written during the run
        <niche>/
          brief.md
          design.png
          listing.json
          upload-receipt.json
          draft.json
```

---

## How to run (dry run, no API keys needed)

```bash
ax claude "Run a print-on-demand drop cycle.
Read starter/niches.json. For each niche:
- Run scripts/image-api.sh to generate the design placeholder
- Run scripts/listing-copy.sh to generate listing copy
- Run scripts/create-draft.sh to produce an unpublished draft
Write all artifacts to runs/merch-drop-dryrun/drops/<niche>/.
After all workers finish, call ax ask with a summary. Stop at drafts." \
  --behavior behaviors/pod.md \
  --no-write \
  --run merch-drop \
  --max-workers 4 \
  --max-depth 1 \
  --max-tokens 2000000 \
  --accept "bash scripts/check-drop.sh runs/merch-drop-dryrun/drops" \
  --dir pod-dryrun/
```

Watch the run in the picker. When the coordinator reaches `ax ask`, the session
shows as "needs you." Reply via the picker (`r`) or `ax reply <id> yes` to
approve, or any other reply to stop at drafts.

---

## How to run (production, real APIs)

```bash
export IMAGE_API_KEY=...
export PRINTIFY_API_TOKEN=...
export PRINTIFY_SHOP_ID=...

ax claude "Run a print-on-demand drop cycle.
Read starter/niches.json. For each niche, launch a worker that:
1. Writes a design brief and calls scripts/image-api.sh to generate a 4500x5400 PNG
2. Calls scripts/listing-copy.sh: title <=140 chars, exactly 13 keyword-front-loaded tags, and a description
3. Calls scripts/create-draft.sh to upload the image and create an UNPUBLISHED product draft
4. Saves all artifacts to runs/merch-drop/drops/<niche>/
After all workers finish and the accept check passes, call ax ask with a summary.
If approved, launch a worker that runs scripts/publish.sh. If not approved or unattended, stop at drafts." \
  --behavior behaviors/pod.md \
  --no-write \
  --run merch-drop \
  --max-workers 4 \
  --max-depth 1 \
  --max-tokens 2000000 \
  --timeout 2h \
  --accept "bash scripts/check-drop.sh runs/merch-drop/drops" \
  --dir pod-dryrun/
```

---

## The pipeline

```
coordinator (fenced, --no-write)
  reads starter/niches.json
  launches N workers in parallel

    worker: niche A             worker: niche B ...
    1. design brief             1. design brief
    2. image-api.sh             2. image-api.sh
    3. listing-copy.sh          3. listing-copy.sh
    4. create-draft.sh          4. create-draft.sh
       (upload + draft)            (upload + draft)

  ax check (check-drop.sh)
  PASS: all niches verified

  ax ask "N drafts ready. Publish?"
  BLOCKS until human replies

    reply: yes           reply: no / timeout
    publish each draft   stop at drafts
```

---

## Human gate

The coordinator calls `ax ask` and blocks. Nothing publishes until a human
replies. In unattended runs (CI, cron), `ax ask` returns immediately without a
reply, so the pipeline always stops at drafts when unattended.

The accept check (`--accept "bash scripts/check-drop.sh <drops-dir>"`) is
enforced mechanically at `ax tag --outcome success`. The coordinator cannot
declare success until every niche has a design, a listing with a valid title
(<=140 chars) and exactly 13 tags, and a draft with `state=draft` and
`published=false`. Those listing limits match Etsy's published caps (140
title characters, 13 tags). Adjust the check for other marketplaces.

---

## Verification status

The mock adapters, the accept check, and the artifact pipeline were tested end
to end without ax (the scripts are plain bash). The coordinator launch flags
are verified against the current ax CLI. The full nested flow (a no-write
coordinator launching real niche workers) was stub-tested at the fan-out step,
not exercised with live nested sessions. The first real run is where you
confirm worker concurrency and the `ax ask` gate behave as described.

---

## Customizing the niche list

Edit `starter/niches.json`. Each entry needs:

```json
{
  "slug": "your-niche-slug",
  "display": "Your Niche Display Name",
  "keywords": ["primary keyword", "secondary keyword"],
  "notes": "optional context for the worker"
}
```

The slug becomes the output directory name. Pass keywords to the worker as
context for the design brief and listing copy.

---

## Adapting the image API

Replace the body of `scripts/image-api.sh` (below the `MOCK:` banner) with a
real adapter. Minimum contract:
- Accept `--niche SLUG --concept "TEXT" --out DIR`
- Write `$out/design.png` (>=4500x5400 px, transparent background)
- Exit 0 on success

---

## Adapting the print-on-demand adapter

Replace the body of `scripts/create-draft.sh` with real curl calls. It maps to
the standard print-on-demand REST API shape in one script:
- upload: POST the image file, receive an image ID (write `upload-receipt.json`)
- create: POST the product with the image ID and listing JSON, set draft state
  and `published=false` (write `draft.json`)

Publishing stays in `scripts/publish.sh`, a separate step the coordinator
reaches only after a human approves via `ax ask`. Even with a real backend
wired in, keep that script gated so nothing publishes without an explicit reply.

---

## Ongoing shop management

Run the same pattern on a schedule:

```bash
ax claude "Read the shop stats export. Identify the top 3 performers and any
listings with zero sales in 60 days. Draft refresh copy for the top 3.
Draft a kill recommendation for zero-sale listings.
Write findings to runs/weekly-review/report.md." \
  --run weekly-review \
  --max-tokens 500000 \
  --timeout 30m \
  --dir pod-dryrun/
```

This one is a plain writable worker, not a fenced coordinator: it writes its
own report and launches nothing.

Wire the run with cron or launchd. The notify hook fires when the run needs a
human decision or completes.
