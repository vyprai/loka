// Detect Vite projects
(function() {
    var configs = ListFiles("vite.config.*");
    return configs.length > 0;
})()
