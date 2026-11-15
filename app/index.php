<?php
// ?mode=direct -> straight to MySQL, ?mode=proxy (default) -> through the Go proxy
$mode = ($_GET['mode'] ?? 'proxy') === 'direct' ? 'DIRECT' : 'PROXY';
$host = getenv("{$mode}_DB_HOST");
$port = getenv("{$mode}_DB_PORT");
$dsn  = "mysql:host=$host;port=$port;dbname=" . getenv('DB_NAME') . ";charset=utf8mb4";

header('Content-Type: application/json');

try {
    $start = microtime(true);
    $pdo = new PDO($dsn, getenv('DB_USER'), getenv('DB_PASS'), [
        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
    ]);

    $value = $pdo->query("SELECT value FROM settings WHERE name = 'site_name'")->fetchColumn();
    $threads = $pdo->query("SHOW STATUS LIKE 'Threads_connected'")->fetch(PDO::FETCH_NUM)[1];

    echo json_encode([
        'mode'              => strtolower($mode),
        'target'            => "$host:$port",
        'site_name'         => $value,
        'threads_connected' => (int) $threads,
        'ms'                => round((microtime(true) - $start) * 1000, 2),
    ]);
} catch (PDOException $e) {
    http_response_code(500);
    echo json_encode(['mode' => strtolower($mode), 'target' => "$host:$port", 'error' => $e->getMessage()]);
}