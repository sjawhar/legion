"""Tests for setup module."""

from pathlib import Path

from legion.setup import get_package_path, install_skills, install_hooks


class TestGetPackagePath:
    def test_returns_path(self) -> None:
        path = get_package_path("skills")
        assert isinstance(path, Path)
        assert path.name == "skills"

    def test_skills_directory_exists(self) -> None:
        path = get_package_path("skills")
        assert path.exists()
        assert path.is_dir()

    def test_hooks_directory_exists(self) -> None:
        path = get_package_path("hooks")
        assert path.exists()
        assert path.is_dir()


class TestInstallSkills:
    def test_installs_to_custom_directory(self, tmp_path: Path) -> None:
        target = tmp_path / "skills"
        installed = install_skills(target)

        assert target.exists()
        assert len(installed) >= 2  # At least controller and worker
        assert "legion-controller" in installed
        assert "legion-worker" in installed

    def test_skill_files_copied(self, tmp_path: Path) -> None:
        target = tmp_path / "skills"
        install_skills(target)

        controller_skill = target / "legion-controller" / "SKILL.md"
        worker_skill = target / "legion-worker" / "SKILL.md"

        assert controller_skill.exists()
        assert worker_skill.exists()

    def test_reinstall_overwrites(self, tmp_path: Path) -> None:
        target = tmp_path / "skills"

        # Install once
        install_skills(target)

        # Modify a file
        marker = target / "legion-controller" / "marker.txt"
        marker.write_text("test")

        # Install again
        install_skills(target)

        # Marker should be gone (directory was replaced)
        assert not marker.exists()


class TestInstallHooks:
    def test_installs_to_workspace(self, tmp_path: Path) -> None:
        workspace = tmp_path / "workspace"
        workspace.mkdir()

        installed = install_hooks(workspace)

        hooks_dir = workspace / ".claude" / "hooks"
        assert hooks_dir.exists()
        assert len(installed) >= 1

    def test_hook_files_executable(self, tmp_path: Path) -> None:
        workspace = tmp_path / "workspace"
        workspace.mkdir()

        install_hooks(workspace)

        hooks_dir = workspace / ".claude" / "hooks"
        for hook_file in hooks_dir.iterdir():
            if hook_file.suffix == ".sh":
                # Check executable bit
                assert hook_file.stat().st_mode & 0o100
