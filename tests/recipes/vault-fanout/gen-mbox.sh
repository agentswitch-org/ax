#!/usr/bin/env bash
# gen-mbox.sh - emit a synthetic mbox with 5 emails across 3 correspondents.
# Usage: ./gen-mbox.sh > fixture/synthetic.mbox
# No dependencies. Safe to re-run (idempotent output).
set -euo pipefail

cat <<'MBOX'
From alice@example.com Thu Jan 01 10:00:00 2026
From: Alice Nakamura <alice@example.com>
To: me@example.com
Date: Thu, 01 Jan 2026 10:00:00 +0000
Subject: Q1 budget review

Hi,

Quick heads-up: I've finished the Q1 budget model. Total headcount spend
comes in at $1.2M. I need your sign-off by Friday so finance can close the
books. I'll attach the spreadsheet after our 3pm call today.

Let me know if you want to walk through it live.

Alice

From bob@example.com Fri Jan 02 09:15:00 2026
From: Bob Okafor <bob@example.com>
To: me@example.com
Date: Fri, 02 Jan 2026 09:15:00 +0000
Subject: Vendor contract renewal - Acme

The Acme contract expires March 15. They came back asking for a 12% price
increase (from $48k/yr to $53.7k/yr). Before I respond I want to understand
whether we're planning to expand use or consolidate. Can you give me a sense
of the roadmap for that product area by end of next week?

Bob

From carol@example.com Fri Jan 02 14:30:00 2026
From: Carol Meier <carol@example.com>
To: me@example.com
Date: Fri, 02 Jan 2026 14:30:00 +0000
Subject: Onboarding schedule - new hire starts Jan 13

Just confirmed: Priya Sharma starts January 13. I'll own weeks 1-2
(orientation, access provisioning, HR docs). I need you to arrange the
technical onboarding for weeks 3-4: codebase walkthrough, architecture
overview, and pairing sessions with the core team.

Please send me a draft agenda by Jan 7.

Carol

From alice@example.com Mon Jan 05 08:45:00 2026
From: Alice Nakamura <alice@example.com>
To: me@example.com
Date: Mon, 05 Jan 2026 08:45:00 +0000
Subject: Re: Q1 budget review - approved

Great news: CFO signed off on the $1.2M headcount budget this morning.
The formal approval doc is in the shared drive under Finance/Q1-2026.
Please make sure your team leads have read-only access before Wednesday.

Alice

From bob@example.com Tue Jan 06 11:00:00 2026
From: Bob Okafor <bob@example.com>
To: me@example.com
Date: Tue, 06 Jan 2026 11:00:00 +0000
Subject: Re: Vendor contract renewal - let's counter at flat rate

After talking to legal I think we can push back on the 12% increase. Our
usage is flat year-over-year, so cost-per-unit went up with no added value.
Recommend we counter at the current rate ($48k) with a 12-month lock-in.
I'll draft the counter-proposal by Thursday if you can review Friday?

Bob
MBOX
