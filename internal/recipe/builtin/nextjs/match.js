// Detect Next.js projects
(function() {
    if (!FileExists("package.json")) return false;
    var pkg = ReadJSON("package.json");
    if (!pkg) return false;
    var deps = pkg.dependencies || {};
    var devDeps = pkg.devDependencies || {};
    return deps["next"] !== undefined || devDeps["next"] !== undefined;
})()
