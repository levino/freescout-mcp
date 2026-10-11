<?php

namespace Modules\Mcp;

/**
 * Input checks for drafts that need no Laravel, so modules/tests can cover
 * them. Writing the draft itself happens in the controller, through
 * FreeScout's models.
 */
class Drafts
{
    // Together, not per file: this is what ends up in one outgoing mail.
    const MAX_TOTAL_BYTES = 20971520;
    const MAX_FILES = 20;

    /**
     * Addresses as a list, from either an array or a comma/semicolon separated
     * string. Only bare addresses are accepted: FreeScout turns "Name <addr>"
     * into a new customer as a side effect, and a draft must not do that.
     *
     * @throws \InvalidArgumentException naming the first unusable entry
     */
    public static function emails($value)
    {
        if ($value === null || $value === '') {
            return [];
        }
        if (is_string($value)) {
            $value = preg_split('/[,;]/', $value);
        }
        if (!is_array($value)) {
            throw new \InvalidArgumentException('addresses must be a list or a comma separated string');
        }

        $emails = [];
        foreach ($value as $entry) {
            if (!is_string($entry)) {
                throw new \InvalidArgumentException('addresses must be strings');
            }
            $entry = trim($entry);
            if ($entry === '') {
                continue;
            }
            if (filter_var($entry, FILTER_VALIDATE_EMAIL) === false) {
                throw new \InvalidArgumentException('not an email address: '.$entry);
            }
            if (!in_array(strtolower($entry), array_map('strtolower', $emails), true)) {
                $emails[] = $entry;
            }
        }

        return $emails;
    }

    /**
     * Checks the attachment list that comes with the uploaded files. The
     * server sends one entry {filename, mime_type} per file, in the order of
     * the files; $sizes are the sizes PHP measured for those files.
     *
     * @return array [['name' => …, 'mime_type' => declared type or ''], …]
     *
     * @throws \InvalidArgumentException before anything is written
     */
    public static function attachments($meta, array $sizes, $maxTotal = self::MAX_TOTAL_BYTES, $maxFiles = self::MAX_FILES)
    {
        if ($meta === null || $meta === '') {
            $meta = [];
        }
        if (!is_array($meta) || array_values($meta) !== $meta) {
            throw new \InvalidArgumentException('attachments must be a list');
        }
        if (count($meta) !== count($sizes)) {
            throw new \InvalidArgumentException(
                count($meta).' attachments described but '.count($sizes).' files received'
            );
        }
        if (count($meta) > $maxFiles) {
            throw new \InvalidArgumentException('at most '.$maxFiles.' attachments per draft');
        }

        $files = [];
        $total = 0;
        foreach ($meta as $i => $item) {
            $label = 'attachment '.($i + 1);
            if (!is_array($item)) {
                throw new \InvalidArgumentException($label.' is not an object');
            }
            $name = self::fileName($item['filename'] ?? '');
            if ($name === '') {
                throw new \InvalidArgumentException($label.' has no filename');
            }
            if ((int) $sizes[$i] <= 0) {
                throw new \InvalidArgumentException($name.' is empty');
            }
            $total += (int) $sizes[$i];
            if ($total > $maxTotal) {
                throw new \InvalidArgumentException(
                    'attachments are larger than '.round($maxTotal / 1048576).' MB together'
                );
            }
            $files[] = [
                'name'      => $name,
                'mime_type' => isset($item['mime_type']) && is_string($item['mime_type']) ? trim($item['mime_type']) : '',
            ];
        }

        return $files;
    }

    /**
     * The last path segment, without control characters. FreeScout cleans the
     * rest (spaces, restricted extensions) in Attachment::create.
     */
    public static function fileName($name)
    {
        if (!is_string($name)) {
            return '';
        }
        $name = str_replace('\\', '/', $name);
        $name = basename($name);
        $name = preg_replace('/[\x00-\x1F\x7F]/u', '', $name);
        $name = trim((string) $name);

        return ($name === '.' || $name === '..') ? '' : $name;
    }

    /**
     * The type sniffed from the bytes, as FreeScout's own upload uses it. The
     * declared type only wins where the bytes are too generic to tell: an
     * Office file sniffs as a zip, a CSV as plain text.
     */
    public static function mimeType($sniffed, $declared = '')
    {
        $sniffed = strtolower(trim((string) $sniffed));
        $declared = strtolower(trim((string) $declared));
        if (!preg_match('#^[\w.+-]+/[\w.+-]+$#', $declared)) {
            $declared = '';
        }
        $generic = ['', 'application/octet-stream', 'application/zip', 'text/plain'];

        if ($declared !== '' && in_array($sniffed, $generic, true)) {
            return $declared;
        }

        return $sniffed !== '' ? $sniffed : 'application/octet-stream';
    }
}
