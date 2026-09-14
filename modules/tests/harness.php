<?php

/**
 * No PHPUnit: this also runs inside the FreeScout container, which ships no
 * dev dependencies.
 */

$GLOBALS['mcp_tests'] = ['checks' => 0, 'failures' => 0];

function check($name, $actual, $expected)
{
    $GLOBALS['mcp_tests']['checks']++;
    if ($actual === $expected) {
        return;
    }
    $GLOBALS['mcp_tests']['failures']++;
    fwrite(STDERR, "FAIL  $name\n  expected: ".var_export($expected, true)."\n  actual:   ".var_export($actual, true)."\n");
}

function check_throws($name, callable $callable, $needle = '')
{
    $GLOBALS['mcp_tests']['checks']++;
    try {
        $callable();
    } catch (\Exception $e) {
        if ($needle !== '' && stripos($e->getMessage(), $needle) === false) {
            $GLOBALS['mcp_tests']['failures']++;
            fwrite(STDERR, "FAIL  $name\n  threw: ".$e->getMessage()."\n  expected the message to mention: $needle\n");
        }

        return;
    }
    $GLOBALS['mcp_tests']['failures']++;
    fwrite(STDERR, "FAIL  $name\n  expected an exception, none was thrown\n");
}

function test_summary()
{
    $state = $GLOBALS['mcp_tests'];
    echo $state['failures'] === 0
        ? "ok\t".$state['checks']." checks passed\n"
        : "FAIL\t".$state['failures']." of ".$state['checks']." checks failed\n";

    return $state['failures'] === 0 ? 0 : 1;
}
