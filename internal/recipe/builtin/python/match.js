// Detect Python projects
(function() {
    return FileExists("requirements.txt") || FileExists("pyproject.toml");
})()
