# Recipe test apparatus

Fixtures, corpus generators, and recorded proof runs for the recipes in
`recipes/`. This tree is git-tracked because it earns its keep re-verifying the
recipes after a change to ax launch flags or verb semantics. It is deliberately
outside `recipes/`: none of it ships in a copyable recipe bundle, and a recipe
stays runnable without it.

| Path | For recipe | What it is |
|---|---|---|
| `behavior-audit/behavior-template.md` | behavior-audit | zero-data output placeholder (the slim script now early-exits instead) |
| `blackboard/producer.md`, `critic.md` | blackboard | role behaviors from the hardened proof run (the shipped recipe uses inline tasks) |
| `blackboard/blackboard-template.json` | blackboard | init file the shipped script now inlines with `echo` |
| `taste-gate/prose-goal.md` | taste-gate | flagship revision goal (seed-mode input) |
| `taste-gate/llmy-paragraph.md` | taste-gate | deliberately LLM-flavored seed draft |
| `taste-gate/contradictory-rubric.md` | taste-gate | rubric that forces the cap path |
| `taste-gate/EVIDENCE.md` | taste-gate | recorded convergence and cap runs |
| `vault-fanout/gen-mbox.sh` | vault-fanout | synthetic mbox generator for a demo corpus |
| `email-triage/gen-maildir.sh` | email-triage | temp maildir with one noise and one important message |

To reproduce a proof, copy the fixture next to the recipe and run the recipe's
entry script. Each recipe's `recipe.md` names the command.
