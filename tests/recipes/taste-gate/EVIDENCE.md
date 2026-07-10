# Verified behavior

Two empirical runs of `taste-loop.sh` against real `ax claude` workers. Each
producer and reviewer ran headless on subscription auth, bound with
`--max-tokens 120000` and `--no-write`. Regenerate with the commands below.

## Convergence run

Improve-a-draft mode. The seed is the LLM-flavored paragraph in
`fixtures/llmy-paragraph.md`. The goal is `fixtures/prose-goal.md`. Rubric is
`rubrics/prose-flat-register.md`. Threshold 92, cap 5.

```
bash taste-loop.sh \
  --task-file fixtures/prose-goal.md \
  --seed fixtures/llmy-paragraph.md \
  --rubric rubrics/prose-flat-register.md \
  --threshold 92 --max-iter 5
```

Score trajectory:

| iteration | score | pass | violations |
|---|---|---|---|
| 1 (raw seed) | 3 | false | 14 |
| 2 (revised) | 93 | true | 0 |

Iteration 1 scored the raw seed at 3 and quoted 14 offending spans, including
"Here is the exciting part:", "supercharges", "It is fast, flexible, and fun.",
"you unlock", "seamless, powerful", "and the best part?", "At the end of the day",
and "Pick one and ship it." The producer revised against those violations and
iteration 2 scored 93, clearing the threshold. Exit code 0.

Final deliverable:

```
ax runs terminal sessions in tracked background windows. It does not require a
terminal multiplexer, though it can use one. Each session is given a meaningful
name, and ax reports the state of every session.
```

The loop prompted no human between iterations. The script contains no `ax ask`
call. The human sees only the passing result.

## Cap run

Produce-from-scratch mode against `fixtures/contradictory-rubric.md`, a rubric
that penalizes any deliverable over 10 words and any deliverable under 40 words.
No word count satisfies both, so the threshold is unreachable. Threshold 85,
cap 3.

```
bash taste-loop.sh \
  --task "Write a two-sentence factual description of ax, ..." \
  --rubric fixtures/contradictory-rubric.md \
  --threshold 85 --max-iter 3
```

Score trajectory:

| iteration | score | pass |
|---|---|---|
| 1 | 0 | false |
| 2 | 40 | false |
| 3 | 0 | false |

No iteration cleared the threshold. At the cap the loop emitted the best attempt
(iteration 2, score 40) marked NOT PASSING, listed the remaining violation, and
exited with code 2. It did not loop forever and did not fake a pass.

Final output began:

```
NOT PASSING: reached the iteration cap (3) without clearing the threshold (85).
Best attempt was iteration 2 with score 40.
```
