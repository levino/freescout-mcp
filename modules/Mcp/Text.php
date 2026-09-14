<?php

namespace Modules\Mcp;

/**
 * The two conversions between what the model writes and what FreeScout stores.
 * Deliberately free of Laravel and of FreeScout so it can be tested with plain
 * PHP, which is where the fiddly cases live.
 */
class Text
{
    /**
     * Model -> FreeScout. The model sends plain text, FreeScout stores and
     * mails HTML. Escaping first is what keeps a customer address like
     * "<script>" from becoming markup in the outgoing mail.
     */
    public static function toHtml($body)
    {
        $body = trim((string) $body);
        if ($body === '') {
            return '';
        }

        return nl2br(htmlspecialchars($body, ENT_QUOTES | ENT_SUBSTITUTE, 'UTF-8'), false);
    }

    /**
     * FreeScout -> model. Mail HTML is noisy; the model only needs the words.
     * Block ends become newlines so paragraphs do not run into each other.
     */
    public static function toPlainText($html)
    {
        $text = (string) $html;
        $text = preg_replace('/<(script|style)\b[^>]*>.*?<\/\1>/is', '', $text);
        $text = preg_replace('/<br\s*\/?>/i', "\n", $text);
        $text = preg_replace('/<\/(p|div|tr|li|h[1-6]|blockquote)>/i', "\n", $text);
        $text = strip_tags($text);
        $text = html_entity_decode($text, ENT_QUOTES | ENT_HTML5, 'UTF-8');
        // Collapse the runs of blank lines that quoted mail leaves behind.
        $text = preg_replace("/[ \t]+\n/", "\n", $text);
        $text = preg_replace("/\n{3,}/", "\n\n", $text);

        return trim($text);
    }
}
