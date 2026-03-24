// Detect Node.js projects (fallback for any project with package.json)
(function() {
    return FileExists("package.json");
})()
