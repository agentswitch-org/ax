# Rubric: flat developer-doc register

Scores prose on how close it sits to terse factual developer documentation. The
target is the register of FFmpeg and NVIDIA CUDA sample docs. State what a thing
is and what it does, plainly. No selling, no hooks, no flourish.

This rubric exists to kill LLM-flavored prose. The patterns below are the ones
that mark writing as machine-generated or marketing-adjacent. Each one costs
points.

## What to penalize

Each pattern present costs points. A single instance is a violation. Repeated
instances compound.

1. **Rhetorical openers.** Sentences that announce excitement or tee up a reveal.
   "Here is the exciting part." "But here is where it gets interesting." "The
   best part?" Quote the opener. Heavy penalty.

2. **Marketing verbs and adjectives.** Words that sell rather than describe:
   supercharge, unlock, seamless, effortless, powerful, blazing, delightful,
   game-changing, robust (as a boast), rich (as a boast). Quote each one. Heavy
   penalty per instance.

3. **Cutesy or hooky endings.** Sentences engineered to land a beat: "never a
   requirement", "makes X feel native", "names that mean something", "and that
   changes everything", a one-word sentence for effect. Quote the ending. Heavy
   penalty.

4. **Cutesy imperatives.** Punchy commands aimed at the reader for rhythm: "Pick
   one." "Ship it." "Try it." "You do the math." Quote it. Medium penalty.

5. **Dense word salad.** Stacked abstractions and buzzwords that carry little
   information: "leverage synergies across the stack", "a paradigm for holistic
   orchestration". If a sentence sounds impressive but does not say what a thing
   is or does, it fails. Quote it. Medium to heavy penalty by density.

6. **Filler intensifiers and throat-clearing.** "It's worth noting that", "at the
   end of the day", "simply", "just", "really", "very", "truly", "essentially",
   when they add no information. Quote each. Light penalty per instance, compounds
   fast.

7. **Em-dashes and semicolons.** Em-dashes read as LLM punctuation. Semicolons
   read as dressed-up prose. Both are banned in this register. Quote each. Medium
   penalty per instance.

8. **Rule-of-three padding.** Triads assembled for cadence rather than content:
   "it is fast, flexible, and fun". If the third item is there for rhythm, it
   fails. Quote it. Light penalty.

## What to reward

- Sentences that state what a thing is or does, and stop.
- Concrete nouns and verbs. A reader learns a fact per sentence.
- Short declaratives. Plain word order. No windup.
- Technical precision over persuasion.

## Scoring guidance

Start at 100. Subtract for each violation:

- Heavy pattern (items 1, 2, 3, and dense cases of 5): subtract 15 to 25 each.
- Medium pattern (items 4, 7, and lighter cases of 5): subtract 8 to 15 each.
- Light pattern (items 6, 8): subtract 3 to 8 each, and let them compound.

Floor the score at 0. A paragraph with two heavy openers, a marketing verb, and a
hooky ending should land in the 20s or 30s on the first pass. Clean flat prose
with no violations scores 90 to 100.

Set `pass` to true only when `score` is at or above the threshold and no heavy
pattern remains. A single surviving heavy pattern forces `pass` false regardless
of score.

## Suggested threshold

85. High enough to force out every heavy pattern and most medium ones. Reachable
in two to four producer iterations from a heavily LLM-flavored first draft.
