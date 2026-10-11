<?php

Route::group([
    'middleware' => ['mcp.pod_local'],
    'prefix'     => 'mcp-bridge',
    'namespace'  => 'Modules\Mcp\Http\Controllers',
], function () {
    Route::get('mailboxes', 'BridgeController@mailboxes');
    Route::get('users', 'BridgeController@users');
    Route::get('conversations', 'BridgeController@conversations');
    Route::get('conversations/{id}', 'BridgeController@conversation');
    Route::post('conversations/{id}/reply', 'BridgeController@reply');
    Route::post('conversations/{id}/note', 'BridgeController@note');
    Route::post('conversations/{id}/status', 'BridgeController@status');
    Route::post('conversations/{id}/assign', 'BridgeController@assign');
    Route::post('conversations/{id}/draft', 'BridgeController@createReplyDraft');
    Route::post('drafts', 'BridgeController@createConversationDraft');
    Route::post('drafts/{id}', 'BridgeController@updateDraft');
    Route::post('drafts/{id}/delete', 'BridgeController@deleteDraft');
    Route::get('attachments/{id}', 'BridgeController@attachment');
});
