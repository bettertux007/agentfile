# JSON Schema v1 & IDE Autocomplete

`agentfile` embeds Draft 2020-12 JSON Schemas in the binary (`schemas/embed.go`) for `agentfile doctor`, `agentfile audit`, `agentfile diff`, and `agentfile add`/`remove`:

* `schemas/v1/agentfile.schema.json` — `Agentfile.yaml` (`version: 1`, including `extends:`)
* `schemas/v1/agentfile-config.schema.json` — `.agentfile/config.yaml` and `~/.config/agentfile/config.yaml` (`version: 1`)
* `schemas/v1/agentfile-lock.schema.json` — `Agentfile.lock` (`version: 1`, including `extends` and `dependencies`)

## YAML Language Server Modeline

Add a `# yaml-language-server: $schema=...` comment at the top of `Agentfile.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bettertux007/agentfile/main/schemas/v1/agentfile.schema.json
version: 1
targets:
  - claude
  - opencode
```

Or for `.agentfile/config.yaml` and `~/.config/agentfile/config.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bettertux007/agentfile/main/schemas/v1/agentfile-config.schema.json
version: 1
link_mode: copy
on_conflict: ask
git_exclude: true
load_global: true
```

## VS Code Settings (`.vscode/settings.json`)

If you use the Red Hat YAML extension (`redhat.vscode-yaml`), map the schemas in `.vscode/settings.json`:

```json
{
  "yaml.schemas": {
    "https://raw.githubusercontent.com/bettertux007/agentfile/main/schemas/v1/agentfile.schema.json": [
      "Agentfile.yaml"
    ],
    "https://raw.githubusercontent.com/bettertux007/agentfile/main/schemas/v1/agentfile-config.schema.json": [
      ".agentfile/config.yaml"
    ],
    "https://raw.githubusercontent.com/bettertux007/agentfile/main/schemas/v1/agentfile-lock.schema.json": [
      "Agentfile.lock"
    ]
  }
}
```
