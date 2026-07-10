# Rubric: <name>

<One or two sentences. What this rubric scores and what the target looks like
when it is right. Name a concrete reference the reviewer can anchor on.>

## What to penalize

List the patterns that cost points. Be specific enough that the reviewer can
quote an offending span and tie it to a numbered item. Vague criteria produce
vague violations.

1. **<pattern name>.** <What it looks like. Give example spans the reviewer can
   pattern-match against.> <Weight: heavy | medium | light.>
2. **<pattern name>.** <...>
3. **<pattern name>.** <...>

## What to reward

- <What good output does. State it as observable properties of the text, not
  intentions.>
- <...>

## Scoring guidance

Start at 100. Subtract for each violation:

- Heavy pattern: subtract <N> to <M> each.
- Medium pattern: subtract <N> to <M> each.
- Light pattern: subtract <N> to <M> each, compounding.

Floor the score at 0. State roughly where a bad first draft should land and where
clean output should land, so the reviewer calibrates.

Set `pass` to true only when `score` is at or above the threshold and no <heavy
pattern> remains.

## Suggested threshold

<N>. <One sentence on why this number, and how many producer iterations a bad
first draft should need to reach it.>
