<?php

/**
 * Runs every plain-PHP test in this directory.
 *
 *   php modules/tests/run.php
 */

require __DIR__.'/harness.php';
require __DIR__.'/../Mcp/Text.php';
require __DIR__.'/../Mcp/Env.php';
require __DIR__.'/../Zitadel/Oidc.php';

require __DIR__.'/text_test.php';
require __DIR__.'/env_test.php';
require __DIR__.'/oidc_test.php';

exit(test_summary());
