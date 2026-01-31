"""Setup skills and hooks for Legion."""

import shutil
from pathlib import Path


def get_package_path(subpath: str) -> Path:
    """Get path to a file within the legion package."""
    return Path(__file__).parent / subpath


def install_skills(target_dir: Path | None = None) -> list[str]:
    """Install Legion skills to ~/.claude/skills/.

    Returns list of installed skill names.
    """
    if target_dir is None:
        target_dir = Path.home() / ".claude" / "skills"

    target_dir.mkdir(parents=True, exist_ok=True)

    skills_src = get_package_path("skills")
    installed = []

    for skill_dir in skills_src.iterdir():
        if skill_dir.is_dir() and (skill_dir / "SKILL.md").exists():
            dest = target_dir / skill_dir.name
            if dest.exists():
                shutil.rmtree(dest)
            shutil.copytree(skill_dir, dest)
            installed.append(skill_dir.name)

    return installed


def install_hooks(workspace: Path) -> list[str]:
    """Install Legion hooks to a workspace's .claude/hooks/.

    Returns list of installed hook names.
    """
    hooks_dir = workspace / ".claude" / "hooks"
    hooks_dir.mkdir(parents=True, exist_ok=True)

    hooks_src = get_package_path("hooks")
    installed = []

    for hook_file in hooks_src.iterdir():
        if hook_file.is_file() and hook_file.suffix == ".sh":
            dest = hooks_dir / hook_file.name
            shutil.copy2(hook_file, dest)
            dest.chmod(0o755)
            installed.append(hook_file.name)

    return installed


def install_settings(workspace: Path) -> None:
    """Install/update .claude/settings.json for Legion hooks."""
    import json

    settings_path = workspace / ".claude" / "settings.json"
    settings_path.parent.mkdir(parents=True, exist_ok=True)

    # Load existing or create new
    if settings_path.exists():
        settings = json.loads(settings_path.read_text())
    else:
        settings = {}

    # Ensure hooks config exists
    if "hooks" not in settings:
        settings["hooks"] = {}

    # Add PostToolUse hook for Edit|Write
    settings["hooks"]["PostToolUse"] = [
        {
            "matcher": "Edit|Write",
            "hooks": [
                {
                    "type": "command",
                    "command": '"$CLAUDE_PROJECT_DIR"/.claude/hooks/post-tool-use.sh',
                }
            ],
        }
    ]

    settings_path.write_text(json.dumps(settings, indent=2) + "\n")
