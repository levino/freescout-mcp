<?php

use Modules\Mcp\Drafts;

check('no addresses', Drafts::emails(null), []);
check('empty string', Drafts::emails(''), []);
check('comma separated', Drafts::emails('a@example.org, b@example.org'), ['a@example.org', 'b@example.org']);
check('semicolons too', Drafts::emails('a@example.org;b@example.org'), ['a@example.org', 'b@example.org']);
check('a list', Drafts::emails([' a@example.org ', '']), ['a@example.org']);
check('duplicates collapse, case-insensitively', Drafts::emails(['A@example.org', 'a@example.org']), ['A@example.org']);
check_throws('no display names: they would create customers', function () {
    Drafts::emails(['Kirsten Kundin <kundin@example.com>']);
}, 'not an email address');
check_throws('garbage is refused', function () {
    Drafts::emails('kein-mail');
}, 'kein-mail');
check_throws('numbers are refused', function () {
    Drafts::emails([42]);
});

check('no attachments', Drafts::attachments(null, []), []);
check(
    'names and declared types are kept',
    Drafts::attachments([['filename' => 'angebot.pdf', 'mime_type' => 'application/pdf']], [100]),
    [['name' => 'angebot.pdf', 'mime_type' => 'application/pdf']]
);
check(
    'a path is cut down to the file name',
    Drafts::attachments([['filename' => '../../etc/passwd']], [1]),
    [['name' => 'passwd', 'mime_type' => '']]
);
check_throws('descriptions and files must match', function () {
    Drafts::attachments([['filename' => 'a.txt']], []);
}, 'files received');
check_throws('a file needs a name', function () {
    Drafts::attachments([['filename' => '  ']], [3]);
}, 'no filename');
check_throws('empty files are refused', function () {
    Drafts::attachments([['filename' => 'a.txt']], [0]);
}, 'empty');
check_throws('the total counts, not each file', function () {
    Drafts::attachments([['filename' => 'a'], ['filename' => 'b']], [600, 600], 1000);
}, 'MB together');
check_throws('too many files', function () {
    Drafts::attachments([['filename' => 'a'], ['filename' => 'b']], [1, 1], 1000, 1);
}, 'at most 1');
check_throws('an object is not a list', function () {
    Drafts::attachments(['x' => ['filename' => 'a']], [1]);
}, 'must be a list');

check('dot names are no names', Drafts::fileName('..'), '');
check('windows paths too', Drafts::fileName('C:\\Users\\levin\\plan.pdf'), 'plan.pdf');
check('control characters go', Drafts::fileName("pl\x00an.pdf"), 'plan.pdf');
check('umlauts stay', Drafts::fileName('Grüße.txt'), 'Grüße.txt');

check('sniffed type wins', Drafts::mimeType('application/pdf', 'image/png'), 'application/pdf');
check('office files sniff as zip', Drafts::mimeType('application/zip', 'application/vnd.openxmlformats-officedocument.wordprocessingml.document'), 'application/vnd.openxmlformats-officedocument.wordprocessingml.document');
check('csv sniffs as text', Drafts::mimeType('text/plain', 'text/csv'), 'text/csv');
check('nothing known', Drafts::mimeType('', ''), 'application/octet-stream');
check('a broken declared type is ignored', Drafts::mimeType('', 'not a type'), 'application/octet-stream');
