<?php

// The web group on purpose: these routes need a session (state and PKCE
// verifier live there) and CSRF protection for the form posts around them.
Route::group([
    'middleware' => ['web'],
    'prefix'     => 'zitadel',
    'namespace'  => 'Modules\Zitadel\Http\Controllers',
], function () {
    Route::get('login', 'LoginController@start')->name('zitadel.login');
    Route::get('callback', 'LoginController@callback')->name('zitadel.callback');
});
