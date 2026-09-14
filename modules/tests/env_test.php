<?php

use Modules\Mcp\Env;

check('a plain assignment', Env::parse('FOO=bar')['FOO'], 'bar');
check('spaces around the equals sign', Env::parse('FOO = bar')['FOO'], 'bar');
check('an empty value stays empty', Env::parse('FOO=')['FOO'], '');
check('double quotes are removed', Env::parse('FOO="bar baz"')['FOO'], 'bar baz');
check('single quotes are removed', Env::parse("FOO='bar baz'")['FOO'], 'bar baz');
check('escaped newlines inside double quotes', Env::parse('FOO="a\nb"')['FOO'], "a\nb");
check('an escaped quote inside double quotes', Env::parse('FOO="say \"hi\""')['FOO'], 'say "hi"');
check('a value may contain an equals sign', Env::parse('FOO=a=b')['FOO'], 'a=b');
check('the export prefix is ignored', Env::parse('export FOO=bar')['FOO'], 'bar');
check('comments are skipped', array_key_exists('FOO', Env::parse('# FOO=bar')), false);
check('blank lines are skipped', count(Env::parse("\n\n  \n")), 0);
check('a line without an equals sign is skipped', count(Env::parse('JUST_A_WORD')), 0);
check('an invalid key is skipped', count(Env::parse('1FOO=bar')), 0);
check('later lines win', Env::parse("FOO=one\nFOO=two")['FOO'], 'two');
check('carriage returns do not end up in the value', Env::parse("FOO=bar\r\nBAZ=qux")['FOO'], 'bar');
check('several keys', count(Env::parse("A=1\nB=2\nC=3")), 3);

$realistic = <<<'ENV'
APP_KEY=base64:abc123==
# our settings
MCP_BRIDGE_TOKEN="a-token-with spaces"
ZITADEL_ISSUER=https://id.levinkeller.de
ZITADEL_FORCE_LOGIN=true
ENV;
$parsed = Env::parse($realistic);
check('a realistic file: the token', $parsed['MCP_BRIDGE_TOKEN'], 'a-token-with spaces');
check('a realistic file: the issuer', $parsed['ZITADEL_ISSUER'], 'https://id.levinkeller.de');
check('a realistic file: base64 values keep their padding', $parsed['APP_KEY'], 'base64:abc123==');

Env::setValues(['SET' => 'yes', 'EMPTY' => '', 'TRUTHY' => 'true', 'FALSY' => 'false']);
check('get returns a value', Env::get('SET'), 'yes');
check('get falls back for a missing key', Env::get('MISSING', 'fallback'), 'fallback');
check('an empty value counts as missing', Env::get('EMPTY', 'fallback'), 'fallback');
check('bool reads true', Env::bool('TRUTHY'), true);
check('bool reads false', Env::bool('FALSY'), false);
check('bool falls back', Env::bool('MISSING', true), true);
Env::setValues(null);
