<?php

namespace Modules\Mcp\Http\Controllers;

use App\Attachment;
use App\Conversation;
use App\Customer;
use App\Events\UserCreatedConversationDraft;
use App\Events\UserCreatedConversation;
use App\Events\UserCreatedThreadDraft;
use App\Events\UserReplied;
use App\Folder;
use App\Mailbox;
use App\Thread;
use App\User;
use Illuminate\Http\Request;
use Illuminate\Routing\Controller;
use Modules\Mcp\Drafts;
use Modules\Mcp\Text;

class BridgeController extends Controller
{
    const MAX_LIMIT = 100;
    const MAX_ATTACHMENT_BYTES = 10485760;

    public function mailboxes(Request $request)
    {
        $user = $this->actingUser($request);
        if ($user instanceof \Illuminate\Http\JsonResponse) {
            return $user;
        }

        return response()->json(['mailboxes' => $this->visibleMailboxes($user)->map(function ($mailbox) {
            return [
                'id'    => $mailbox->id,
                'name'  => $mailbox->name,
                'email' => $mailbox->email,
            ];
        })->values()]);
    }

    public function users(Request $request)
    {
        if (($error = $this->actingUserOrError($request)) !== null) {
            return $error;
        }

        $users = User::where('status', User::STATUS_ACTIVE)->orderBy('email')->get();

        return response()->json(['users' => $users->map(function ($user) {
            return [
                'id'    => $user->id,
                'email' => $user->email,
                'name'  => $user->getFullName(),
                'admin' => $user->isAdmin(),
            ];
        })->values()]);
    }

    /**
     * Search through FreeScout's own Conversation::search, the query behind
     * its search page: subject, customer, number, and every message's text,
     * sender and recipients.
     */
    public function conversations(Request $request)
    {
        $user = $this->actingUser($request);
        if ($user instanceof \Illuminate\Http\JsonResponse) {
            return $user;
        }

        $limit = min((int) $request->input('limit', 25) ?: 25, self::MAX_LIMIT);
        $offset = max((int) $request->input('offset', 0), 0);

        $filters = [];
        if ($mailboxId = (int) $request->input('mailbox_id')) {
            if (!$this->visibleMailboxes($user)->contains('id', $mailboxId)) {
                return response()->json(['error' => 'no access to this mailbox'], 403);
            }
            $filters['mailbox'] = $mailboxId;
        }
        if ($status = $this->statusValue($request->input('status'))) {
            $filters['status'] = [$status];
        }
        $filters['state'] = [$request->input('state') === 'draft' ? Conversation::STATE_DRAFT : Conversation::STATE_PUBLISHED];
        foreach (['after', 'before'] as $bound) {
            $value = trim((string) $request->input($bound, ''));
            if ($value === '') {
                continue;
            }
            if (!preg_match('/^\d{4}-\d{2}-\d{2}$/', $value) || strtotime($value) === false) {
                return response()->json(['error' => $bound.' must be a date like 2026-10-31'], 422);
            }
            $filters[$bound] = $value;
        }
        if ($user->canSeeOnlyAssignedConversations()) {
            $filters['assigned'] = $user->id;
        }

        $query = Conversation::search(trim((string) $request->input('query', '')), $filters, $user);

        if ($from = trim((string) $request->input('from', ''))) {
            $query->where('threads.from', 'like', '%'.mb_strtolower($from).'%');
        }
        if ($to = trim((string) $request->input('to', ''))) {
            $like = '%'.mb_strtolower($to).'%';
            $query->where(function ($q) use ($like) {
                $q->where('threads.to', 'like', $like)->orWhere('threads.cc', 'like', $like);
            });
        }

        // The query is grouped by conversation, so count the groups.
        $total = \DB::table(\DB::raw('('.$query->toSql().') as matches'))
            ->mergeBindings($query->getQuery())
            ->count();
        $conversations = $query->skip($offset)->take($limit)->get();

        return response()->json([
            'total'         => $total,
            'offset'        => $offset,
            'conversations' => $conversations->map(function ($conversation) {
                return $this->summarize($conversation);
            })->values(),
        ]);
    }

    public function conversation(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }

        $threads = $conversation->threads()->orderBy('created_at', 'asc')->get();

        return response()->json([
            'conversation' => $this->summarize($conversation) + [
                'threads' => $threads->map(function ($thread) {
                    $isDraft = $thread->state == Thread::STATE_DRAFT;

                    return [
                        'id'          => $thread->id,
                        'type'        => $this->threadTypeName($thread->type),
                        // An unsent draft; its id is the draft_id for update_draft.
                        'draft'       => $isDraft,
                        'from'        => $thread->source_via == Thread::PERSON_USER ? 'user' : 'customer',
                        'author'      => $thread->created_by_user_id
                            ? optional(User::find($thread->created_by_user_id))->email
                            : $thread->from,
                        'created_at'  => (string) $thread->created_at,
                        'body'        => $this->toPlainText($thread->body),
                        'attachments' => $this->attachmentsOf($thread),
                    ] + ($isDraft ? [
                        'to'  => $thread->getToArray(),
                        'cc'  => $thread->getCcArray(),
                        'bcc' => $thread->getBccArray(),
                    ] : []);
                })->values(),
            ],
        ]);
    }

    /**
     * Sends a reply. What Conversation::createUserThread does, plus cc, bcc
     * and attachments the way the reply form's send_reply sets them; the
     * files are stored before UserReplied fires, so they go out with the mail.
     */
    public function reply(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }
        $user = $this->actingUser($request);

        if ($conversation->state != Conversation::STATE_PUBLISHED) {
            return response()->json(['error' => 'this conversation is an unsent draft, send it with send_draft'], 422);
        }
        $input = $this->draftInput($request, ['body', 'cc', 'bcc', 'attachments']);
        if ($input instanceof \Illuminate\Http\JsonResponse) {
            return $input;
        }
        if ($input['body'] === '') {
            return response()->json(['error' => 'empty body'], 422);
        }
        $status = $this->statusValue($this->draftPayload($request)['status'] ?? null);

        $thread = $this->transaction(function () use ($conversation, $user, $input, $status) {
            $now = date('Y-m-d H:i:s');
            $conversation->setPreview($input['body']);
            $conversation->last_reply_at = $now;
            $conversation->last_reply_from = Conversation::PERSON_USER;
            $conversation->user_updated_at = $now;
            if (count($input['cc'])) {
                $conversation->setCc(array_values(array_diff($input['cc'], [$conversation->customer_email])));
            }
            if (count($input['bcc'])) {
                $conversation->setBcc($input['bcc']);
            }
            if (count($input['attachments'])) {
                $conversation->has_attachments = true;
            }
            if ($status) {
                $conversation->setStatus($status, $user);
            } else {
                $conversation->updateFolder();
            }
            $conversation->save();

            $thread = Thread::create($conversation, Thread::TYPE_MESSAGE, $input['body'], [], false);
            $thread->source_via = Thread::PERSON_USER;
            $thread->source_type = Thread::SOURCE_TYPE_WEB;
            $thread->user_id = $conversation->user_id;
            $thread->status = $conversation->status;
            $thread->state = Thread::STATE_PUBLISHED;
            $thread->customer_id = $conversation->customer_id;
            $thread->created_by_user_id = $user->id;
            $thread->edited_by_user_id = null;
            $thread->edited_at = null;
            $thread->setTo($conversation->customer_email);
            $thread->setCc($input['cc']);
            $thread->setBcc($input['bcc']);
            $thread->has_attachments = count($input['attachments']) > 0;
            $thread->save();

            $this->storeAttachments($input['attachments'], $thread, $user);

            return $thread;
        });
        if ($thread instanceof \Illuminate\Http\JsonResponse) {
            return $thread;
        }

        $conversation->mailbox->updateFoldersCounters();
        event(new UserReplied($conversation, $thread));
        \Eventy::action('conversation.user_replied', $conversation, $thread);

        return response()->json([
            'ok'           => true,
            'thread_id'    => $thread->id,
            'attachments'  => $this->attachmentsOf($thread->fresh()),
            'conversation' => $this->summarize($conversation->fresh()),
        ]);
    }

    /**
     * Sends a draft the acting user wrote — the from_draft path of FreeScout's
     * send_reply: the draft thread itself becomes the published message, a
     * new-conversation draft gets its customer and becomes a conversation.
     */
    public function sendDraft(Request $request, $id)
    {
        $thread = $this->findOwnDraft($request, $id);
        if ($thread instanceof \Illuminate\Http\JsonResponse) {
            return $thread;
        }
        $user = $this->actingUser($request);
        $conversation = $thread->conversation;
        $isNewConversation = $conversation->state == Conversation::STATE_DRAFT;

        if ($thread->subtype == Thread::SUBTYPE_FORWARD) {
            return response()->json(['error' => 'a forward draft is sent from FreeScout, or use forward_conversation with send'], 422);
        }
        if (trim(Text::toPlainText($thread->body)) === '') {
            return response()->json(['error' => 'the draft has no text'], 422);
        }
        $to = $thread->getToArray();
        if ($isNewConversation && !count($to)) {
            return response()->json(['error' => 'the draft has no recipient'], 422);
        }
        if ($isNewConversation && trim((string) $conversation->subject) === '') {
            return response()->json(['error' => 'the draft has no subject'], 422);
        }
        $status = $this->statusValue($request->input('status'));

        $prevState = $conversation->state;
        $sent = $this->transaction(function () use ($thread, $conversation, $user, $isNewConversation, $to, $status) {
            $now = date('Y-m-d H:i:s');
            $cc = $thread->getCcArray();

            if ($isNewConversation) {
                // First recipient is the customer, the others go to Cc.
                $customerEmail = $to[0];
                $customer = Customer::create($customerEmail);
                $conversation->customer_id = $customer->id;
                $conversation->customer_email = $customerEmail;
                $cc = array_values(array_diff($cc, [$customerEmail]));
                if (count($to) > 1) {
                    $cc = array_values(array_unique(array_merge($cc, array_diff($to, [$customerEmail]))));
                }
                $conversation->setCc($cc);
                $conversation->state = Conversation::STATE_PUBLISHED;
                if ($status) {
                    $conversation->status = $status;
                }
                if ($conversation->isClosed()) {
                    $conversation->closed_by_user_id = $user->id;
                    $conversation->closed_at = $now;
                }
                $thread->first = true;
            } else {
                $conversation->setCc(array_values(array_diff($cc, [$conversation->customer_email])));
                if ($status) {
                    $conversation->setStatus($status, $user);
                }
            }
            $conversation->setBcc($thread->getBccArray());
            $conversation->last_reply_at = $now;
            $conversation->last_reply_from = Conversation::PERSON_USER;
            $conversation->user_updated_at = $now;
            if ($thread->has_attachments) {
                $conversation->has_attachments = true;
            }
            $conversation->updateFolder();
            // A thread published from a draft is not counted by ThreadObserver.
            $conversation->threads_count++;
            $conversation->setPreview($thread->body);
            $conversation->save();

            $thread->type = Thread::TYPE_MESSAGE;
            $thread->created_at = $now;
            $thread->user_id = $conversation->user_id;
            $thread->status = $conversation->status;
            $thread->state = Thread::STATE_PUBLISHED;
            $thread->customer_id = $conversation->customer_id;
            $thread->created_by_user_id = $user->id;
            $thread->edited_by_user_id = null;
            $thread->edited_at = null;
            $thread->save();

            return $thread;
        });
        if ($sent instanceof \Illuminate\Http\JsonResponse) {
            return $sent;
        }

        $conversation->maybeRemoveFromDrafts();
        $conversation->mailbox->updateFoldersCounters();
        if ($conversation->state != $prevState) {
            \Eventy::action('conversation.state_changed', $conversation, $user, $prevState);
        }

        if ($isNewConversation) {
            event(new UserCreatedConversation($conversation, $thread));
            \Eventy::action('conversation.created_by_user', $conversation, $thread);
        } else {
            if ($conversation->user_id && $conversation->user_id != $user->id) {
                $user->followConversation($conversation->id);
            }
            event(new UserReplied($conversation, $thread));
            \Eventy::action('conversation.user_replied', $conversation, $thread);
        }

        return response()->json([
            'ok'           => true,
            'sent'         => true,
            'thread_id'    => $thread->id,
            'conversation' => $this->summarize($conversation->fresh()),
        ]);
    }

    /**
     * Forwards a conversation. As a draft, it is what the Forward button saves:
     * a draft thread with SUBTYPE_FORWARD in this conversation, the
     * conversation's files copied onto it. Sent right away, it is FreeScout's
     * own Conversation::forward.
     */
    public function forward(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }
        $user = $this->actingUser($request);
        if ($conversation->state != Conversation::STATE_PUBLISHED) {
            return response()->json(['error' => 'an unsent draft cannot be forwarded'], 422);
        }

        $input = $this->draftInput($request, ['to', 'cc', 'bcc', 'body', 'attachments']);
        if ($input instanceof \Illuminate\Http\JsonResponse) {
            return $input;
        }
        if (!count($input['to'])) {
            return response()->json(['error' => 'no recipient'], 422);
        }
        $payload = $this->draftPayload($request);
        $send = !empty($payload['send']);
        $includeAttachments = !array_key_exists('include_attachments', $payload) || !empty($payload['include_attachments']);

        if ($send) {
            if (count($input['to']) != 1) {
                return response()->json(['error' => 'forward to one address at a time when sending right away'], 422);
            }
            if (count($input['attachments'])) {
                return response()->json(['error' => 'extra attachments only on a forward draft; send it from there'], 422);
            }
            $before = Thread::where('conversation_id', $conversation->id)->max('id');
            $conversation->forward($user, $input['body'], $input['to'][0], ['cc' => $input['cc'], 'bcc' => $input['bcc']], $includeAttachments);

            $note = Thread::where('conversation_id', $conversation->id)
                ->where('id', '>', (int) $before)
                ->where('subtype', Thread::SUBTYPE_FORWARD)
                ->orderBy('id', 'desc')
                ->first();
            $childId = $note ? $note->getMeta(Thread::META_FORWARD_CHILD_CONVERSATION_ID) : null;

            return response()->json([
                'ok'                        => true,
                'sent'                      => true,
                'forwarded_conversation_id' => $childId,
                'conversation'              => $this->summarize($conversation->fresh()),
            ]);
        }

        return $this->writeDraft(function () use ($conversation, $user, $input, $includeAttachments) {
            $thread = new Thread();
            $thread->conversation_id = $conversation->id;
            $thread->user_id = $user->id;
            $thread->type = Thread::TYPE_MESSAGE;
            $thread->subtype = Thread::SUBTYPE_FORWARD;
            $thread->state = Thread::STATE_DRAFT;
            $thread->source_via = Thread::PERSON_USER;
            $thread->source_type = Thread::SOURCE_TYPE_WEB;
            if ($conversation->customer_id) {
                $thread->customer_id = $conversation->customer_id;
            }
            $thread->created_by_user_id = $user->id;
            $thread->from = null;
            $thread->body = $input['body'];
            $thread->setTo($input['to']);
            $thread->setCc($input['cc']);
            $thread->setBcc($input['bcc']);
            $thread->save();

            if ($includeAttachments) {
                // What load_attachments does for a forward: copies, so the
                // original files survive whatever happens to the draft.
                foreach ($conversation->getReplies() as $reply) {
                    foreach ($reply->attachments as $attachment) {
                        $attachment->duplicate($thread->id);
                    }
                }
            }
            $this->storeAttachments($input['attachments'], $thread, $user);
            $thread->has_attachments = Attachment::where('thread_id', $thread->id)->where('embedded', false)->exists();
            $thread->save();

            $conversation->addToFolder(Folder::TYPE_DRAFTS);
            $conversation->mailbox->updateFoldersCounters(Folder::TYPE_DRAFTS);
            event(new UserCreatedThreadDraft($conversation, $thread));

            return $thread;
        });
    }

    /**
     * Moves a conversation to another mailbox — Conversation::moveToMailbox,
     * what the Move menu calls.
     */
    public function move(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }
        $user = $this->actingUser($request);

        $mailbox = Mailbox::find((int) $request->input('mailbox_id'));
        if (!$mailbox) {
            return response()->json(['error' => 'mailbox not found'], 404);
        }
        if (!$this->visibleMailboxes($user)->contains('id', $mailbox->id)) {
            return response()->json(['error' => 'no access to the target mailbox'], 403);
        }
        if ($mailbox->id == $conversation->mailbox_id) {
            return response()->json(['error' => 'the conversation is already in this mailbox'], 422);
        }

        $conversation->moveToMailbox($mailbox, $user);

        return response()->json(['ok' => true, 'conversation' => $this->summarize($conversation->fresh())]);
    }

    /**
     * Merges other conversations into this one — Conversation::mergeConversations,
     * what the Merge dialog calls. The others end up in Deleted.
     */
    public function merge(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }
        $user = $this->actingUser($request);

        $ids = $request->input('merge_ids');
        if (!is_array($ids) || !count($ids)) {
            return response()->json(['error' => 'merge_ids must be a non-empty list'], 422);
        }
        $others = [];
        foreach (array_unique(array_map('intval', $ids)) as $otherId) {
            if ($otherId == $conversation->id) {
                return response()->json(['error' => 'a conversation cannot be merged with itself'], 422);
            }
            $other = $this->findConversation($request, $otherId);
            if ($other instanceof \Illuminate\Http\JsonResponse) {
                return response()->json(['error' => '#'.$otherId.': '.$other->getData(true)['error']], $other->status());
            }
            if ($other->state != Conversation::STATE_PUBLISHED) {
                return response()->json(['error' => '#'.$otherId.' cannot be merged (draft or deleted)'], 422);
            }
            $others[] = $other;
        }

        foreach ($others as $other) {
            $conversation->mergeConversations($other, $user);
        }

        return response()->json([
            'ok'           => true,
            'merged'       => array_map(function ($c) { return $c->id; }, $others),
            'conversation' => $this->summarize($conversation->fresh()),
        ]);
    }

    /**
     * Customers by name, company or address, with their latest conversations
     * in the mailboxes the acting user can see.
     */
    public function customers(Request $request)
    {
        $user = $this->actingUser($request);
        if ($user instanceof \Illuminate\Http\JsonResponse) {
            return $user;
        }
        $q = trim((string) $request->input('query', ''));
        if (mb_strlen($q) < 2) {
            return response()->json(['error' => 'query needs at least two characters'], 422);
        }
        $like = '%'.mb_strtolower($q).'%';
        $mailboxIds = $this->visibleMailboxes($user)->pluck('id')->all();
        $limited = config('app.limit_user_customer_visibility') && !$user->isAdmin();

        $customers = Customer::where(function ($w) use ($like) {
            $w->where('first_name', 'like', $like)
                ->orWhere('last_name', 'like', $like)
                ->orWhere(\DB::raw('CONCAT(first_name, " ", last_name)'), 'like', $like)
                ->orWhere('company', 'like', $like)
                ->orWhereIn('id', function ($sub) use ($like) {
                    $sub->select('customer_id')->from('emails')->where('email', 'like', $like);
                });
        })->orderBy('id', 'desc')->limit(20)->get();

        $result = [];
        foreach ($customers as $customer) {
            $conversations = Conversation::where('customer_id', $customer->id)
                ->whereIn('mailbox_id', $mailboxIds)
                ->where('state', Conversation::STATE_PUBLISHED)
                ->orderBy('last_reply_at', 'desc')
                ->limit(10)
                ->get();
            if ($limited && !count($conversations)) {
                continue;
            }
            $result[] = [
                'id'            => $customer->id,
                'name'          => $customer->getFullName(),
                'emails'        => $customer->emails->pluck('email')->values(),
                'company'       => $customer->company,
                'job_title'     => $customer->job_title,
                'phones'        => $customer->getPhones(),
                'conversations' => $conversations->map(function ($conversation) {
                    return $this->summarize($conversation);
                })->values(),
            ];
        }

        return response()->json(['customers' => $result]);
    }

    public function note(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }
        $user = $this->actingUser($request);

        $body = $this->bodyHtml($request->input('body'));
        if ($body === '') {
            return response()->json(['error' => 'empty body'], 422);
        }

        $conversation->createUserThread($user, $body, ['type' => Thread::TYPE_NOTE]);

        return response()->json(['ok' => true]);
    }

    public function status(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }
        $user = $this->actingUser($request);

        $status = $this->statusValue($request->input('status'));
        if (!$status) {
            return response()->json(['error' => 'unknown status'], 422);
        }

        $conversation->setStatus($status, $user);
        $conversation->save();
        $conversation->mailbox->updateFoldersCounters();

        return response()->json(['ok' => true, 'conversation' => $this->summarize($conversation->fresh())]);
    }

    public function assign(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }
        $user = $this->actingUser($request);

        $assigneeEmail = trim((string) $request->input('assignee_email', ''));
        if ($assigneeEmail === '') {
            $newUserId = null;
        } else {
            $assignee = User::where('email', $assigneeEmail)->where('status', User::STATUS_ACTIVE)->first();
            if (!$assignee) {
                return response()->json(['error' => 'unknown assignee'], 422);
            }
            $newUserId = $assignee->id;
        }

        $conversation->changeUser($newUserId, $user);
        $conversation->mailbox->updateFoldersCounters();

        return response()->json(['ok' => true, 'conversation' => $this->summarize($conversation->fresh())]);
    }

    /**
     * A reply that waits in FreeScout for a person to send it. Mirrors the
     * reply branch of FreeScout's own `save_draft` (ConversationsController):
     * a thread in STATE_DRAFT, the conversation in the Drafts folder, nothing
     * queued for delivery.
     */
    public function createReplyDraft(Request $request, $id)
    {
        $conversation = $this->findConversation($request, $id);
        if ($conversation instanceof \Illuminate\Http\JsonResponse) {
            return $conversation;
        }
        $user = $this->actingUser($request);

        if ($conversation->state != Conversation::STATE_PUBLISHED) {
            return response()->json(['error' => 'this conversation is itself an unsent draft, change it with update_draft'], 422);
        }

        $input = $this->draftInput($request, ['body', 'cc', 'bcc', 'attachments']);
        if ($input instanceof \Illuminate\Http\JsonResponse) {
            return $input;
        }
        if ($input['body'] === '') {
            return response()->json(['error' => 'empty body'], 422);
        }

        return $this->writeDraft(function () use ($conversation, $user, $input) {
            $conversation->updateFolder();
            $conversation->save();

            $thread = new Thread();
            $thread->conversation_id = $conversation->id;
            $thread->user_id = $user->id;
            $thread->type = Thread::TYPE_MESSAGE;
            $thread->state = Thread::STATE_DRAFT;
            $thread->source_via = Thread::PERSON_USER;
            $thread->source_type = Thread::SOURCE_TYPE_WEB;
            if ($conversation->customer_id) {
                $thread->customer_id = $conversation->customer_id;
            }
            $thread->created_by_user_id = $user->id;
            $thread->from = null;
            $thread->body = $input['body'];
            $thread->setTo($conversation->customer_email);
            $thread->setCc($input['cc']);
            $thread->setBcc($input['bcc']);
            $thread->has_attachments = count($input['attachments']) > 0;
            $thread->save();

            $this->storeAttachments($input['attachments'], $thread, $user);

            $conversation->addToFolder(Folder::TYPE_DRAFTS);
            $conversation->mailbox->updateFoldersCounters(Folder::TYPE_DRAFTS);

            event(new UserCreatedThreadDraft($conversation, $thread));

            return $thread;
        });
    }

    /**
     * A new conversation that is not sent yet. Mirrors the "New Conversation"
     * branch of `save_draft`: conversation and first thread both in
     * STATE_DRAFT, recipients on the thread, no customer until it is sent.
     */
    public function createConversationDraft(Request $request)
    {
        $user = $this->actingUser($request);
        if ($user instanceof \Illuminate\Http\JsonResponse) {
            return $user;
        }

        $mailbox = Mailbox::find((int) ($this->draftPayload($request)['mailbox_id'] ?? 0));
        if (!$mailbox) {
            return response()->json(['error' => 'mailbox not found'], 404);
        }
        if (!$this->visibleMailboxes($user)->contains('id', $mailbox->id)) {
            return response()->json(['error' => 'no access to this mailbox'], 403);
        }

        $input = $this->draftInput($request, ['to', 'cc', 'bcc', 'subject', 'body', 'attachments']);
        if ($input instanceof \Illuminate\Http\JsonResponse) {
            return $input;
        }
        if (!count($input['to'])) {
            return response()->json(['error' => 'no recipient'], 422);
        }
        if ($input['subject'] === '') {
            return response()->json(['error' => 'empty subject'], 422);
        }
        if ($input['body'] === '') {
            return response()->json(['error' => 'empty body'], 422);
        }
        $hasAttachments = count($input['attachments']) > 0;

        // One address is stored as a string, several as a list; on sending,
        // FreeScout then offers one conversation per recipient.
        $to = count($input['to']) == 1 ? $input['to'][0] : $input['to'];

        return $this->writeDraft(function () use ($mailbox, $user, $input, $hasAttachments, $to) {
            $conversation = new Conversation();
            $conversation->type = Conversation::TYPE_EMAIL;
            $conversation->state = Conversation::STATE_DRAFT;
            $conversation->status = $this->newConversationStatus($mailbox);
            $conversation->subject = $input['subject'];
            $conversation->setPreview($input['body']);
            if ($hasAttachments) {
                $conversation->has_attachments = true;
            }
            $conversation->mailbox_id = $mailbox->id;
            $conversation->customer_email = '';
            $conversation->created_by_user_id = $user->id;
            $conversation->source_via = Conversation::PERSON_USER;
            $conversation->source_type = Conversation::SOURCE_TYPE_WEB;
            $conversation->setCc($input['cc']);
            $conversation->setBcc($input['bcc']);
            $conversation->updateFolder();
            $conversation->save();

            $thread = new Thread();
            $thread->conversation_id = $conversation->id;
            $thread->user_id = $user->id;
            $thread->first = true;
            $thread->type = Thread::TYPE_MESSAGE;
            $thread->state = Thread::STATE_DRAFT;
            $thread->source_via = Thread::PERSON_USER;
            $thread->source_type = Thread::SOURCE_TYPE_WEB;
            $thread->created_by_user_id = $user->id;
            $thread->from = null;
            $thread->body = $input['body'];
            $thread->setTo($to);
            $thread->setCc($input['cc']);
            $thread->setBcc($input['bcc']);
            $thread->has_attachments = $hasAttachments;
            $thread->save();

            $this->storeAttachments($input['attachments'], $thread, $user);

            $conversation->addToFolder(Folder::TYPE_DRAFTS);
            $mailbox->updateFoldersCounters(Folder::TYPE_DRAFTS);

            event(new UserCreatedConversationDraft($conversation, $thread));

            return $thread;
        });
    }

    /**
     * Changes a draft the acting user wrote. Fields that are not given stay as
     * they are; attachments are added, and removed by id.
     */
    public function updateDraft(Request $request, $id)
    {
        $thread = $this->findOwnDraft($request, $id);
        if ($thread instanceof \Illuminate\Http\JsonResponse) {
            return $thread;
        }
        $user = $this->actingUser($request);
        $conversation = $thread->conversation;
        $isNewConversation = $conversation->state == Conversation::STATE_DRAFT;

        $fields = ['body', 'cc', 'bcc', 'attachments'];
        if ($isNewConversation) {
            $fields[] = 'to';
            $fields[] = 'subject';
        } elseif ($this->draftHas($request, 'to') || $this->draftHas($request, 'subject')) {
            return response()->json(['error' => 'to and subject belong to the conversation, a reply draft only has body, cc, bcc and attachments'], 422);
        }
        $input = $this->draftInput($request, $fields);
        if ($input instanceof \Illuminate\Http\JsonResponse) {
            return $input;
        }

        $remove = $this->draftPayload($request)['remove_attachment_ids'] ?? [];
        if (!is_array($remove)) {
            return response()->json(['error' => 'remove_attachment_ids must be a list'], 422);
        }
        $remove = array_map('intval', $remove);
        if (count($remove)) {
            $own = Attachment::where('thread_id', $thread->id)->whereIn('id', $remove)->pluck('id')->all();
            if (count($own) != count(array_unique($remove))) {
                return response()->json(['error' => 'remove_attachment_ids names an attachment that is not on this draft'], 422);
            }
        }

        if ($this->draftHas($request, 'body')) {
            if ($input['body'] === '') {
                return response()->json(['error' => 'empty body'], 422);
            }
            $thread->body = $input['body'];
            if ($isNewConversation) {
                $conversation->setPreview($input['body']);
            }
        }
        if ($isNewConversation && $this->draftHas($request, 'to')) {
            if (!count($input['to'])) {
                return response()->json(['error' => 'no recipient'], 422);
            }
            $thread->setTo(count($input['to']) == 1 ? $input['to'][0] : $input['to']);
        }
        if ($isNewConversation && $this->draftHas($request, 'subject')) {
            if ($input['subject'] === '') {
                return response()->json(['error' => 'empty subject'], 422);
            }
            $conversation->subject = $input['subject'];
        }
        if ($this->draftHas($request, 'cc')) {
            $thread->setCc($input['cc']);
            if ($isNewConversation) {
                $conversation->setCc($input['cc']);
            }
        }
        if ($this->draftHas($request, 'bcc')) {
            $thread->setBcc($input['bcc']);
            if ($isNewConversation) {
                $conversation->setBcc($input['bcc']);
            }
        }

        return $this->writeDraft(function () use ($remove, $input, $thread, $user, $conversation, $isNewConversation) {
            $this->storeAttachments($input['attachments'], $thread, $user);
            // Last, because it removes files from disk, which no rollback undoes.
            if (count($remove)) {
                Attachment::deleteByIds($remove);
            }

            $hasAttachments = Attachment::where('thread_id', $thread->id)->where('embedded', false)->exists();
            $thread->has_attachments = $hasAttachments;
            $thread->save();
            if ($isNewConversation) {
                $conversation->has_attachments = $hasAttachments;
            }
            $conversation->save();

            return $thread;
        });
    }

    /**
     * Discards a draft the acting user wrote, the way FreeScout's own
     * `discard_draft` does: a new-conversation draft takes its conversation
     * with it, a reply draft only itself.
     */
    public function deleteDraft(Request $request, $id)
    {
        $thread = $this->findOwnDraft($request, $id);
        if ($thread instanceof \Illuminate\Http\JsonResponse) {
            return $thread;
        }
        $user = $this->actingUser($request);
        $conversation = $thread->conversation;
        $mailbox = $conversation->mailbox;

        if ($conversation->state == Conversation::STATE_DRAFT) {
            $conversation->removeFromFolder(Folder::TYPE_DRAFTS);
            $conversation->removeFromFolder(Folder::TYPE_STARRED, $user->id);
            $mailbox->updateFoldersCounters(Folder::TYPE_DRAFTS);
            $conversation->deleteThreads();
            $conversation->delete();

            Conversation::clearStarredByUserCache($user->id, $mailbox->id);
            $mailbox->updateFoldersCounters(Folder::TYPE_STARRED);

            return response()->json(['ok' => true, 'deleted' => 'conversation', 'conversation_id' => $conversation->id]);
        }

        $thread->deleteThread();
        if ($conversation->maybeRemoveFromDrafts()) {
            $mailbox->updateFoldersCounters(Folder::TYPE_DRAFTS);
        }

        return response()->json(['ok' => true, 'deleted' => 'draft', 'conversation_id' => $conversation->id]);
    }

    /**
     * Runs writes in one transaction, so a failure (a full disk, say) leaves
     * nothing half-written behind. Returns the callable's result, or an error
     * response.
     */
    protected function transaction(callable $write)
    {
        \DB::beginTransaction();
        try {
            $result = $write();
            \DB::commit();
        } catch (\Throwable $e) {
            \DB::rollBack();
            \Log::error('[mcp] writing failed: '.$e->getMessage());

            return response()->json(['error' => 'the helpdesk could not store this'], 500);
        }

        return $result;
    }

    protected function writeDraft(callable $write)
    {
        $thread = $this->transaction($write);
        if ($thread instanceof \Illuminate\Http\JsonResponse) {
            return $thread;
        }

        return response()->json(['ok' => true, 'draft' => $this->describeDraft($thread->fresh())]);
    }

    protected function findOwnDraft(Request $request, $id)
    {
        $user = $this->actingUser($request);
        if ($user instanceof \Illuminate\Http\JsonResponse) {
            return $user;
        }

        $thread = Thread::find((int) $id);
        if (!$thread || !$thread->conversation) {
            return response()->json(['error' => 'draft not found'], 404);
        }
        if (!$this->visibleMailboxes($user)->contains('id', $thread->conversation->mailbox_id)) {
            return response()->json(['error' => 'no access to this mailbox'], 403);
        }
        if ($thread->state != Thread::STATE_DRAFT) {
            return response()->json(['error' => 'this is not a draft (any more), it may have been sent already'], 409);
        }
        if ((int) $thread->created_by_user_id !== (int) $user->id) {
            return response()->json(['error' => 'this draft belongs to someone else'], 403);
        }

        return $thread;
    }

    /**
     * Validates everything before anything is written, so a bad address or an
     * oversized file never leaves half a draft behind.
     */
    protected function draftInput(Request $request, array $fields)
    {
        $payload = $this->draftPayload($request);
        if ($payload === null) {
            return response()->json(['error' => 'the draft fields did not arrive as JSON'], 422);
        }

        $input = ['body' => '', 'subject' => '', 'to' => [], 'cc' => [], 'bcc' => [], 'attachments' => []];
        try {
            foreach (['to', 'cc', 'bcc'] as $field) {
                if (in_array($field, $fields, true)) {
                    try {
                        $input[$field] = Drafts::emails($payload[$field] ?? null);
                    } catch (\InvalidArgumentException $e) {
                        throw new \InvalidArgumentException($field.': '.$e->getMessage());
                    }
                }
            }
            if (in_array('attachments', $fields, true)) {
                $input['attachments'] = $this->uploadedAttachments($request, $payload['attachments'] ?? []);
            }
        } catch (\InvalidArgumentException $e) {
            return response()->json(['error' => $e->getMessage()], 422);
        }
        if (in_array('body', $fields, true)) {
            $input['body'] = $this->bodyHtml($payload['body'] ?? '');
        }
        if (in_array('subject', $fields, true)) {
            $subject = is_string($payload['subject'] ?? null) ? $payload['subject'] : '';
            $input['subject'] = mb_substr(trim(preg_replace('/\s+/u', ' ', $subject)), 0, 998);
        }

        return $input;
    }

    /**
     * Draft routes take multipart/form-data: the fields as JSON in `payload`,
     * the files as `files[]`. Files arrive as real uploads, so PHP streams
     * them to disk instead of holding megabytes of base64 in memory, and
     * Attachment::create stores them exactly like an upload from the editor.
     */
    protected function draftPayload(Request $request)
    {
        $payload = json_decode((string) $request->input('payload', ''), true);

        return is_array($payload) ? $payload : null;
    }

    protected function draftHas(Request $request, $field)
    {
        $payload = $this->draftPayload($request);

        return is_array($payload) && array_key_exists($field, $payload);
    }

    protected function uploadedAttachments(Request $request, $meta)
    {
        $uploads = $request->file('files');
        if ($uploads === null) {
            $uploads = [];
        } elseif (!is_array($uploads)) {
            $uploads = [$uploads];
        }
        $uploads = array_values($uploads);

        $sizes = [];
        foreach ($uploads as $i => $upload) {
            if (!$upload || !$upload->isValid()) {
                throw new \InvalidArgumentException('file '.($i + 1).' did not arrive');
            }
            $sizes[] = (int) $upload->getSize();
        }

        $files = Drafts::attachments($meta, $sizes);
        foreach ($files as $i => $file) {
            $files[$i]['upload'] = $uploads[$i];
            $files[$i]['mime_type'] = Drafts::mimeType($uploads[$i]->getMimeType(), $file['mime_type']);
        }

        return $files;
    }

    /**
     * What FreeScout's upload does, minus the HTTP upload: Attachment::create
     * writes the file to its disk (storage/app/attachment/…) and the row to
     * `attachments`, here already bound to the draft thread.
     */
    protected function storeAttachments(array $files, Thread $thread, User $user)
    {
        foreach ($files as $file) {
            $attachment = Attachment::create(
                $file['name'],
                $file['mime_type'],
                null,
                '',
                $file['upload'],
                false,
                $thread->id,
                $user->id
            );
            if (!$attachment) {
                \Log::error('[mcp] storing attachment '.$file['name'].' for draft '.$thread->id.' failed');
            }
        }
    }

    protected function describeDraft(Thread $thread)
    {
        $conversation = $thread->conversation;

        return [
            'draft_id'         => $thread->id,
            'conversation_id'  => $conversation->id,
            'new_conversation' => $conversation->state == Conversation::STATE_DRAFT,
            'mailbox_id'       => $conversation->mailbox_id,
            'subject'          => $conversation->subject,
            'to'               => $thread->getToArray(),
            'cc'               => $thread->getCcArray(),
            'bcc'              => $thread->getBccArray(),
            'body'             => $this->toPlainText($thread->body),
            'attachments'      => Attachment::where('thread_id', $thread->id)->orderBy('id')->get()->map(function ($attachment) {
                return $this->describeAttachment($attachment);
            })->values(),
            'sent'             => false,
            'url'              => $conversation->url(),
        ];
    }

    protected function newConversationStatus(Mailbox $mailbox)
    {
        $status = (int) $mailbox->ticket_status;
        $valid = [Conversation::STATUS_ACTIVE, Conversation::STATUS_PENDING, Conversation::STATUS_CLOSED];

        return in_array($status, $valid, true) ? $status : Conversation::STATUS_ACTIVE;
    }

    protected function actingUser(Request $request)
    {
        $email = trim((string) $request->input('as_email', $request->header('X-Mcp-Acting-User', '')));
        if ($email === '') {
            return response()->json(['error' => 'no acting user'], 422);
        }

        $user = User::where('email', $email)->where('status', User::STATUS_ACTIVE)->first();
        if (!$user) {
            return response()->json(['error' => 'unknown acting user'], 403);
        }

        return $user;
    }

    protected function actingUserOrError(Request $request)
    {
        $user = $this->actingUser($request);

        return $user instanceof \Illuminate\Http\JsonResponse ? $user : null;
    }

    protected function visibleMailboxes(User $user)
    {
        return $user->isAdmin() ? Mailbox::all() : $user->mailboxes()->get();
    }

    protected function findConversation(Request $request, $id)
    {
        $user = $this->actingUser($request);
        if ($user instanceof \Illuminate\Http\JsonResponse) {
            return $user;
        }

        $conversation = Conversation::find((int) $id);
        if (!$conversation) {
            return response()->json(['error' => 'not found'], 404);
        }
        if (!$this->visibleMailboxes($user)->contains('id', $conversation->mailbox_id)) {
            return response()->json(['error' => 'no access to this mailbox'], 403);
        }

        return $conversation;
    }

    public function attachment(Request $request, $id)
    {
        $user = $this->actingUser($request);
        if ($user instanceof \Illuminate\Http\JsonResponse) {
            return $user;
        }

        $attachment = Attachment::find((int) $id);
        if (!$attachment || !$attachment->thread) {
            return response()->json(['error' => 'not found'], 404);
        }

        $conversation = $attachment->thread->conversation;
        if (!$conversation || !$this->visibleMailboxes($user)->contains('id', $conversation->mailbox_id)) {
            return response()->json(['error' => 'no access to this mailbox'], 403);
        }
        if (!$attachment->fileExists()) {
            return response()->json(['error' => 'the file is gone from storage'], 410);
        }
        if ((int) $attachment->size > self::MAX_ATTACHMENT_BYTES) {
            return response()->json([
                'error' => 'attachment is larger than '.round(self::MAX_ATTACHMENT_BYTES / 1048576).' MB',
            ], 413);
        }

        try {
            $contents = $attachment->getFileContents();
        } catch (\Exception $e) {
            \Log::error('[mcp] reading attachment '.$attachment->id.' failed: '.$e->getMessage());

            return response()->json(['error' => 'the file could not be read'], 500);
        }

        return response()->json([
            'attachment' => $this->describeAttachment($attachment) + [
                'conversation_id' => $conversation->id,
                'base64'          => base64_encode($contents),
            ],
        ]);
    }

    protected function attachmentsOf($thread)
    {
        if (!$thread->has_attachments) {
            return [];
        }

        return Attachment::where('thread_id', $thread->id)->get()
            ->map(function ($attachment) {
                return $this->describeAttachment($attachment);
            })->values();
    }

    protected function describeAttachment(Attachment $attachment)
    {
        return [
            'id'        => $attachment->id,
            'name'      => $attachment->file_name,
            'mime_type' => $attachment->mime_type,
            'size'      => (int) $attachment->size,
            'embedded'  => (bool) $attachment->embedded,
        ];
    }

    protected function summarize(Conversation $conversation)
    {
        return [
            'id'             => $conversation->id,
            'number'         => $conversation->number,
            'subject'        => $conversation->subject,
            'status'         => $this->statusName($conversation->status),
            'mailbox_id'     => $conversation->mailbox_id,
            'customer_email' => $conversation->customer_email,
            'assignee'       => $conversation->user_id ? optional(User::find($conversation->user_id))->email : null,
            'threads_count'  => $conversation->threads_count,
            'last_reply_at'  => (string) $conversation->last_reply_at,
            'last_reply_from' => $conversation->last_reply_from == Conversation::PERSON_USER ? 'user' : 'customer',
            'preview'        => $this->toPlainText($conversation->preview),
            'draft'          => $conversation->state == Conversation::STATE_DRAFT,
            'url'            => $conversation->url(),
        ];
    }

    protected function statusValue($name)
    {
        $map = [
            'active'  => Conversation::STATUS_ACTIVE,
            'pending' => Conversation::STATUS_PENDING,
            'closed'  => Conversation::STATUS_CLOSED,
            'spam'    => Conversation::STATUS_SPAM,
        ];

        return $map[strtolower(trim((string) $name))] ?? null;
    }

    protected function statusName($value)
    {
        $map = [
            Conversation::STATUS_ACTIVE  => 'active',
            Conversation::STATUS_PENDING => 'pending',
            Conversation::STATUS_CLOSED  => 'closed',
            Conversation::STATUS_SPAM    => 'spam',
        ];

        return $map[$value] ?? (string) $value;
    }

    protected function threadTypeName($value)
    {
        $map = [
            Thread::TYPE_CUSTOMER => 'customer',
            Thread::TYPE_MESSAGE  => 'message',
            Thread::TYPE_NOTE     => 'note',
            Thread::TYPE_LINEITEM => 'lineitem',
            Thread::TYPE_CHAT     => 'chat',
        ];

        return $map[$value] ?? (string) $value;
    }

    protected function bodyHtml($body)
    {
        return Text::toHtml($body);
    }

    protected function toPlainText($html)
    {
        return Text::toPlainText($html);
    }
}
