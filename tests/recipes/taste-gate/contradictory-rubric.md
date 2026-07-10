# Rubric: contradictory (cap-path test only)

This rubric is a test artifact. It states two requirements that no deliverable can
satisfy at once. It exists to prove that the loop stops at the iteration cap and
reports honestly instead of looping forever or faking a pass. Do not use it for
real work.

## What to penalize

1. **Too long.** Penalize any deliverable of more than 10 words. This is a heavy
   violation. Quote the text. Subtract 60.
2. **Too short.** Penalize any deliverable of fewer than 40 words. This is a heavy
   violation. Quote the text. Subtract 60.

No word count satisfies both item 1 and item 2. At least one is always violated.

## Scoring guidance

Start at 100. Apply every violated item. Because item 1 and item 2 cannot both be
satisfied, the score can never clear a threshold above 40, and a heavy violation
always remains, so `pass` must be false on every iteration.

Set `pass` to false always. This rubric cannot be passed by design.

## Suggested threshold

85. Unreachable here on purpose. The loop must exhaust its cap and report the best
attempt marked NOT PASSING.
