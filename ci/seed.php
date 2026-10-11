<?php

/**
 * Fixture for the end-to-end stack, run inside the FreeScout container:
 *
 *   docker compose exec -T freescout php /ci/seed.php
 *
 * The fields mirror what FetchEmails sets for an incoming message; a
 * conversation assembled differently would test a shape production never has.
 */

require '/www/html/vendor/autoload.php';

$app = require '/www/html/bootstrap/app.php';
$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();

use App\Attachment;
use App\Conversation;
use App\Customer;
use App\Mailbox;
use App\Thread;
use App\User;

$now = date('Y-m-d H:i:s');

// The image creates this user on an empty database, but its bootstrap has been
// seen to segfault on arm64.
$admin = User::where('email', 'post@levinkeller.de')->first();
if (!$admin) {
    $admin = new User();
    $admin->email = 'post@levinkeller.de';
    $admin->first_name = 'Levin';
    $admin->last_name = 'Keller';
    $admin->password = bcrypt('e2e-admin-password-1234');
    $admin->role = User::ROLE_ADMIN;
    $admin->status = User::STATUS_ACTIVE;
    $admin->save();
}

$colleague = User::where('email', 'kollegin@levinkeller.de')->first();
if (!$colleague) {
    $colleague = new User();
    $colleague->email = 'kollegin@levinkeller.de';
    $colleague->first_name = 'Kolle';
    $colleague->last_name = 'Gin';
    $colleague->password = bcrypt('e2e-colleague-password-1234');
    $colleague->role = User::ROLE_USER;
    $colleague->status = User::STATUS_ACTIVE;
    $colleague->save();
}

$mailbox = Mailbox::where('email', 'info@example.org')->first();
if (!$mailbox) {
    $mailbox = Mailbox::create([
        'name'      => 'Ökohaus',
        'email'     => 'info@example.org',
        'from_name' => Mailbox::FROM_NAME_MAILBOX,
    ]);
}
$mailbox->users()->sync([$admin->id, $colleague->id]);
$mailbox->syncPersonalFolders([$admin->id, $colleague->id]);

$customer = Customer::create('kundin@example.com', ['first_name' => 'Kirsten', 'last_name' => 'Kundin']);

$body = '<div>Guten Tag,<br><br>wann kann das Dach geliefert werden?<br><br>Viele Grüße</div>';

$conversation = new Conversation();
$conversation->type = Conversation::TYPE_EMAIL;
$conversation->state = Conversation::STATE_PUBLISHED;
$conversation->status = Conversation::STATUS_ACTIVE;
$conversation->subject = 'Lieferung Dach';
$conversation->setPreview($body);
$conversation->mailbox_id = $mailbox->id;
$conversation->customer_id = $customer->id;
$conversation->created_by_customer_id = $customer->id;
$conversation->customer_email = 'kundin@example.com';
$conversation->source_via = Conversation::PERSON_CUSTOMER;
$conversation->source_type = Conversation::SOURCE_TYPE_EMAIL;
$conversation->setLastReplyAt($now, Conversation::PERSON_CUSTOMER);
$conversation->last_reply_from = Conversation::PERSON_CUSTOMER;
$conversation->created_at = $now;
$conversation->updateFolder();
$conversation->save();

$thread = new Thread();
$thread->conversation_id = $conversation->id;
$thread->user_id = $conversation->user_id;
$thread->type = Thread::TYPE_CUSTOMER;
$thread->status = $conversation->status;
$thread->state = Thread::STATE_PUBLISHED;
$thread->body = $body;
$thread->from = 'kundin@example.com';
$thread->setTo(['info@example.org']);
$thread->source_via = Thread::PERSON_CUSTOMER;
$thread->source_type = Thread::SOURCE_TYPE_EMAIL;
$thread->customer_id = $customer->id;
$thread->created_by_customer_id = $customer->id;
$thread->first = true;
$thread->created_at = $now;
$thread->updated_at = $now;
$thread->save();

$textAttachment = Attachment::create(
    'angebot.txt',
    'text/plain',
    Attachment::TYPE_TEXT,
    "Angebot über 5 m² Dachfläche\nGrüße aus Rössing\n",
    null,
    false,
    $thread->id,
    null
);

$pngBytes = base64_decode(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='
);
$imageAttachment = Attachment::create(
    'dach.png',
    'image/png',
    Attachment::TYPE_IMAGE,
    $pngBytes,
    null,
    false,
    $thread->id,
    null
);

$thread->has_attachments = true;
$thread->save();

// A colleague's unsent reply, the way FreeScout's save_draft leaves it: the
// MCP tools may show it but must not change or discard it.
$colleagueDraft = new Thread();
$colleagueDraft->conversation_id = $conversation->id;
$colleagueDraft->user_id = $colleague->id;
$colleagueDraft->type = Thread::TYPE_MESSAGE;
$colleagueDraft->state = Thread::STATE_DRAFT;
$colleagueDraft->source_via = Thread::PERSON_USER;
$colleagueDraft->source_type = Thread::SOURCE_TYPE_WEB;
$colleagueDraft->customer_id = $customer->id;
$colleagueDraft->created_by_user_id = $colleague->id;
$colleagueDraft->body = 'Entwurf der Kollegin';
$colleagueDraft->setTo('kundin@example.com');
$colleagueDraft->save();
$conversation->addToFolder(App\Folder::TYPE_DRAFTS);

// Two more plain conversations of the same customer, one to merge and one to
// move, and a second mailbox to move it to.
function seed_conversation($mailbox, $customer, $subject, $body)
{
    $now = date('Y-m-d H:i:s');
    $c = new Conversation();
    $c->type = Conversation::TYPE_EMAIL;
    $c->state = Conversation::STATE_PUBLISHED;
    $c->status = Conversation::STATUS_ACTIVE;
    $c->subject = $subject;
    $c->setPreview($body);
    $c->mailbox_id = $mailbox->id;
    $c->customer_id = $customer->id;
    $c->created_by_customer_id = $customer->id;
    $c->customer_email = 'kundin@example.com';
    $c->source_via = Conversation::PERSON_CUSTOMER;
    $c->source_type = Conversation::SOURCE_TYPE_EMAIL;
    $c->setLastReplyAt($now, Conversation::PERSON_CUSTOMER);
    $c->last_reply_from = Conversation::PERSON_CUSTOMER;
    $c->updateFolder();
    $c->save();

    $t = new Thread();
    $t->conversation_id = $c->id;
    $t->type = Thread::TYPE_CUSTOMER;
    $t->status = $c->status;
    $t->state = Thread::STATE_PUBLISHED;
    $t->body = $body;
    $t->from = 'kundin@example.com';
    $t->setTo([$mailbox->email]);
    $t->source_via = Thread::PERSON_CUSTOMER;
    $t->source_type = Thread::SOURCE_TYPE_EMAIL;
    $t->customer_id = $customer->id;
    $t->created_by_customer_id = $customer->id;
    $t->first = true;
    $t->save();

    return $c;
}

$secondMailbox = Mailbox::where('email', 'planung@example.org')->first();
if (!$secondMailbox) {
    $secondMailbox = Mailbox::create([
        'name'      => 'Planung',
        'email'     => 'planung@example.org',
        'from_name' => Mailbox::FROM_NAME_MAILBOX,
    ]);
}
$secondMailbox->users()->sync([$admin->id]);
$secondMailbox->syncPersonalFolders([$admin->id]);

$mergeConversation = seed_conversation($mailbox, $customer, 'Rechnung Dach', '<div>Anbei die Rechnung zum Dach.</div>');
$moveConversation = seed_conversation($mailbox, $customer, 'Fenster Planung', '<div>Wann planen wir die Fenster?</div>');
$secondMailbox->updateFoldersCounters();

$mailbox->updateFoldersCounters();

echo json_encode([
    'mailbox_id'          => $mailbox->id,
    'conversation_id'     => $conversation->id,
    'customer_email'      => 'kundin@example.com',
    'admin_email'         => $admin->email,
    'colleague_email'     => $colleague->email,
    'thread_id'           => $thread->id,
    'text_attachment_id'  => $textAttachment ? $textAttachment->id : null,
    'image_attachment_id' => $imageAttachment ? $imageAttachment->id : null,
    'image_base64'        => base64_encode($pngBytes),
    'colleague_draft_id'  => $colleagueDraft->id,
    'second_mailbox_id'   => $secondMailbox->id,
    'merge_conversation_id' => $mergeConversation->id,
    'move_conversation_id'  => $moveConversation->id,
], JSON_PRETTY_PRINT)."\n";
