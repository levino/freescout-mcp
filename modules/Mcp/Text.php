<?php

namespace Modules\Mcp;

class Text
{
    public static function toHtml($body)
    {
        $body = trim((string) $body);
        if ($body === '') {
            return '';
        }

        return nl2br(htmlspecialchars($body, ENT_QUOTES | ENT_SUBSTITUTE, 'UTF-8'), false);
    }

    public static function toPlainText($html)
    {
        $text = (string) $html;
        $text = preg_replace('/<(script|style)\b[^>]*>.*?<\/\1>/is', '', $text);
        $text = preg_replace('/<br\s*\/?>/i', "\n", $text);
        $text = preg_replace('/<\/(p|div|tr|li|h[1-6]|blockquote)>/i', "\n", $text);
        $text = strip_tags($text);
        $text = html_entity_decode($text, ENT_QUOTES | ENT_HTML5, 'UTF-8');
        $text = preg_replace("/[ \t]+\n/", "\n", $text);
        $text = preg_replace("/\n{3,}/", "\n\n", $text);

        return trim($text);
    }
}
