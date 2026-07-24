# personas/

This folder ships **empty**. FleetChat has no built-in crew — a fresh board
starts **blank**, and you add agents yourself with the **+ Add agent** button
(point it at a project folder and pick a CLI).

To give an agent a fixed identity instead of the auto-generated default, drop a
folder here named by its id:

```
personas/<agent-id>/agent.json    {"name", "id", "role", "intro",  optional "dir", "cli"}
personas/<agent-id>/PERSONA.md     the agent's system prompt
```

- `dir` — the folder the agent runs from (its own repo).
- `cli` — which backend launches it (`claude` | `gemini` | `qwen`, default `claude`).

Keep anything you don't want committed in **`personas.local/`** instead — it's
git-ignored, so your real setup is never pushed. To declare a whole crew at once,
see `fleet.local.example.json`.
