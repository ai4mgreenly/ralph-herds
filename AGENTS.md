# ralph-herds

Process supervisor for the Ralph pipeline. Launches, monitors, and restarts all the Ralph nano-services from a single place.

Like herding cats — except the cats are micro-services, and they mostly cooperate.

## Workflow

All code changes in this repository go through goals — never direct edits. When something needs changing, create a goal and let the pipeline execute it.

The only exception: the user gives extremely explicit instructions to make changes directly. "Extremely explicit" means the user unambiguously requests a direct change — e.g. "edit this file now", "make this change locally", "don't create a goal". Ambiguity defaults to creating a goal.

If you're unsure whether to make a direct change or create a goal, create a goal.

## Architecture

Part of a multi-service system:

| Service | Language | Port | Purpose |
|---------|----------|------|---------|
| **ralph-remembers** | Go | 5001 | Memory and context storage |
| **ralph-plans** | Go + SQLite | 5002 | Goal storage and state machine |
| **ralph-shows** | Deno + Preact | 5000 | Web UI dashboard |
| **ralph-runs** | Ruby | 5002 | Orchestrator + agent loop |
| **ralph-logs** | Go | 5003 | Real-time log streaming |
| **ralph-counts** | Python | 5004 | Metrics dashboard |
| **ralph-herds** | ? | ? | This project — Process supervisor |

### Source Layout

```
ralph-herds/
├── .claude/
│   ├── commands/         # Custom slash commands
│   ├── library/          # Skills (modular instruction sets)
│   └── skillsets/        # Composite skill bundles
└── AGENTS.md             # This file
```

## Development

### Version Control

This project uses **git**.

## Skills

Skills are modular instruction sets in `.claude/library/<name>/SKILL.md`.

- **Load a skill**: `/load <name>` reads the skill into context
- **Load multiple**: `/load name1 name2`

### Skillsets

Composite bundles in `.claude/skillsets/<name>.json`:

```json
{
  "preload": ["skill-a"],
  "advertise": [{"skill": "skill-b", "description": "When to use"}]
}
```

- `preload` — loaded immediately when skillset is activated
- `advertise` — shown as available, loaded on demand with `/load`

Available skillsets:

- `meta` — For improving the .claude/ system (preloads: jj, align)

### For Ralph

When Ralph executes a goal in this repo, it receives only `AGENTS.md` as project context. This file is responsible for getting Ralph everything it needs.
