<?php

namespace Modules\Mcp\Http\Controllers;

use App\Conversation;
use App\Mailbox;
use App\Thread;
use App\User;
use Illuminate\Http\Request;
use Illuminate\Routing\Controller;
use Modules\Mcp\Text;

class BridgeController extends Controller
{
    const MAX_LIMIT = 100;

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

    public function conversations(Request $request)
    {
        $user = $this->actingUser($request);
        if ($user instanceof \Illuminate\Http\JsonResponse) {
            return $user;
        }

        $limit = min((int) $request->input('limit', 25) ?: 25, self::MAX_LIMIT);
        $offset = max((int) $request->input('offset', 0), 0);

        $query = Conversation::whereIn('mailbox_id', $this->visibleMailboxes($user)->pluck('id')->all())
            ->where('state', Conversation::STATE_PUBLISHED);

        if ($status = $this->statusValue($request->input('status'))) {
            $query->where('status', $status);
        }
        if ($mailboxId = (int) $request->input('mailbox_id')) {
            $query->where('mailbox_id', $mailboxId);
        }
        if ($needle = trim((string) $request->input('query', ''))) {
            $like = '%'.$needle.'%';
            $query->where(function ($q) use ($like) {
                $q->where('subject', 'like', $like)
                  ->orWhere('customer_email', 'like', $like)
                  ->orWhere('preview', 'like', $like);
            });
        }

        $total = $query->count();
        $conversations = $query->orderBy('last_reply_at', 'desc')->skip($offset)->take($limit)->get();

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
                    return [
                        'id'          => $thread->id,
                        'type'        => $this->threadTypeName($thread->type),
                        'from'        => $thread->source_via == Thread::PERSON_USER ? 'user' : 'customer',
                        'author'      => $thread->created_by_user_id
                            ? optional(User::find($thread->created_by_user_id))->email
                            : $thread->from,
                        'created_at'  => (string) $thread->created_at,
                        'body'        => $this->toPlainText($thread->body),
                        'attachments' => (int) $thread->has_attachments,
                    ];
                })->values(),
            ],
        ]);
    }

    public function reply(Request $request, $id)
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

        $now = date('Y-m-d H:i:s');
        $conversation->setPreview($body);
        $conversation->last_reply_at = $now;
        $conversation->last_reply_from = Conversation::PERSON_USER;
        $conversation->user_updated_at = $now;

        if ($status = $this->statusValue($request->input('status'))) {
            $conversation->setStatus($status, $user);
        } else {
            $conversation->updateFolder();
        }
        $conversation->save();

        $conversation->createUserThread($user, $body, ['type' => Thread::TYPE_MESSAGE]);

        return response()->json(['ok' => true, 'conversation' => $this->summarize($conversation->fresh())]);
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
