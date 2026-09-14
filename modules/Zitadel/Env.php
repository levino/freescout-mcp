<?php

namespace Modules\Zitadel;

/**
 * Settings from FreeScout's .env, also when the config cache is in place.
 *
 * Laravel skips loading the .env file entirely once bootstrap/cache/config.php
 * exists, and that file exists in every image we run — so Laravel's own env()
 * returns null for anything that is not baked into a config file. Reading the
 * file is the reliable way to see a setting that belongs to a module.
 *
 * Duplicated per module on purpose: the two modules are installed
 * independently, and neither may depend on the other being present.
 */
class Env
{
    private static $values = null;

    public static function get($key, $default = null)
    {
        // function_exists: the helper is also used by the test suite, which
        // runs without a Laravel application.
        $value = function_exists('env') ? env($key, null) : null;
        if ($value !== null && $value !== '') {
            return $value;
        }

        $values = self::values();
        if (array_key_exists($key, $values) && $values[$key] !== '') {
            return $values[$key];
        }

        return $default;
    }

    public static function bool($key, $default = false)
    {
        $value = self::get($key, null);
        if ($value === null) {
            return $default;
        }

        return filter_var($value, FILTER_VALIDATE_BOOLEAN);
    }

    /** Parses the KEY=VALUE lines of a .env file. */
    public static function parse($contents)
    {
        $values = [];
        foreach (preg_split('/\r\n|\r|\n/', (string) $contents) as $line) {
            $line = trim($line);
            if ($line === '' || $line[0] === '#') {
                continue;
            }
            if (strpos($line, 'export ') === 0) {
                $line = trim(substr($line, 7));
            }
            $parts = explode('=', $line, 2);
            if (count($parts) !== 2) {
                continue;
            }
            $key = trim($parts[0]);
            if (!preg_match('/^[A-Za-z_][A-Za-z0-9_.]*$/', $key)) {
                continue;
            }
            $value = trim($parts[1]);
            if (strlen($value) >= 2 && ($value[0] === '"' || $value[0] === "'") && substr($value, -1) === $value[0]) {
                $quote = $value[0];
                $value = substr($value, 1, -1);
                if ($quote === '"') {
                    $value = str_replace(['\\n', '\\"'], ["\n", '"'], $value);
                }
            }
            $values[$key] = $value;
        }

        return $values;
    }

    /** Test seam: lets the suite run without a Laravel application. */
    public static function setValues(?array $values = null)
    {
        self::$values = $values;
    }

    private static function values()
    {
        if (self::$values !== null) {
            return self::$values;
        }

        $path = function_exists('base_path') ? base_path('.env') : '';
        self::$values = ($path && is_readable($path)) ? self::parse(file_get_contents($path)) : [];

        return self::$values;
    }
}
