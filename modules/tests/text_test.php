<?php

use Modules\Mcp\Text;

// --- Text::toHtml: what the model writes, on its way into a mail ------

check('empty stays empty', Text::toHtml('   '), '');
// nl2br(..., false) writes the HTML5 form; FreeScout's own bodies use it too.
check('newlines become breaks', Text::toHtml("a\nb"), "a<br>\nb");
check(
    'markup from the model is escaped, not rendered',
    Text::toHtml('<script>alert(1)</script>'),
    '&lt;script&gt;alert(1)&lt;/script&gt;'
);
check('quotes are escaped', Text::toHtml('sagte "hallo"'), 'sagte &quot;hallo&quot;');
check('umlauts survive', Text::toHtml('Grüße aus Rössing'), 'Grüße aus Rössing');
check('ampersand is escaped once', Text::toHtml('Tür & Tor'), 'Tür &amp; Tor');
check('leading and trailing whitespace is trimmed', Text::toHtml("\n hallo \n"), 'hallo');
check('invalid utf-8 does not blank the message', Text::toHtml("gut\xB1"), 'gut�');

// --- Text::toPlainText: what FreeScout stored, on its way to the model -

check('breaks become newlines', Text::toPlainText('a<br>b'), "a\nb");
check('self-closing breaks too', Text::toPlainText('a<br />b'), "a\nb");
check('paragraphs are separated', Text::toPlainText('<p>eins</p><p>zwei</p>'), "eins\nzwei");
check('tags are stripped', Text::toPlainText('<div><strong>fett</strong></div>'), 'fett');
check('entities are decoded', Text::toPlainText('T&uuml;r &amp; Tor'), 'Tür & Tor');
check('non-breaking spaces survive decoding', Text::toPlainText('a&nbsp;b'), "a\u{00a0}b");
check('scripts are removed with their content', Text::toPlainText('<script>alert(1)</script>text'), 'text');
check('styles are removed with their content', Text::toPlainText('<style>p{color:red}</style>text'), 'text');
check(
    'quoted mail does not turn into a wall of blank lines',
    Text::toPlainText('<p>oben</p><br><br><br><p>unten</p>'),
    "oben\n\nunten"
);
check('list items end up on their own lines', Text::toPlainText('<ul><li>a</li><li>b</li></ul>'), "a\nb");
check('blockquotes end a line', Text::toPlainText('<blockquote>zitat</blockquote>danach'), "zitat\ndanach");
check('empty html is empty text', Text::toPlainText('<p></p>'), '');
check('plain text passes through unchanged', Text::toPlainText('einfach nur Text'), 'einfach nur Text');

// A round trip must not double-escape: what the model wrote is what the next
// read gives back.
$written = 'Angebot über 5 m² für "Haus & Hof"';
check('round trip keeps the text intact', Text::toPlainText(Text::toHtml($written)), $written);
check(
    'round trip keeps line breaks',
    Text::toPlainText(Text::toHtml("Guten Tag,\n\nvielen Dank.")),
    "Guten Tag,\n\nvielen Dank."
);
check(
    'a customer address in angle brackets survives the round trip',
    Text::toPlainText(Text::toHtml('schreiben Sie an <info@example.org>')),
    'schreiben Sie an <info@example.org>'
);
