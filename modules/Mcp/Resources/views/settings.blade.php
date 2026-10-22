<div class="form-group">
    <label class="col-sm-2 control-label">{{ __('Address') }}</label>
    <div class="col-sm-6">
        <input type="text" class="form-control" readonly onclick="this.select()" value="{{ $mcp_endpoint }}">
        <p class="help-block">
            {{ __('Add this address as a connector in Claude. Signing in happens through ZITADEL; the account has to exist here as an active user.') }}
        </p>
    </div>
</div>

<div class="form-group">
    <label class="col-sm-2 control-label">{{ __('Server') }}</label>
    <div class="col-sm-6">
        <p class="form-control-static">
            @if ($mcp_version)
                <i class="glyphicon glyphicon-ok text-success"></i> {{ __('Running') }} ({{ $mcp_version }})
            @else
                <i class="glyphicon glyphicon-remove text-danger"></i> {{ __('Not reachable') }}
            @endif
        </p>
    </div>
</div>

<div class="form-group">
    <label class="col-sm-2 control-label">{{ __('Bridge') }}</label>
    <div class="col-sm-6">
        <p class="form-control-static">
            @if ($mcp_bridge_ready)
                <i class="glyphicon glyphicon-ok text-success"></i> {{ __('Token configured') }}
            @else
                <i class="glyphicon glyphicon-remove text-danger"></i> {{ __('MCP_BRIDGE_TOKEN is missing') }}
            @endif
        </p>
    </div>
</div>

<div class="form-group">
    <label class="col-sm-2 control-label">{{ __('Tools') }}</label>
    <div class="col-sm-6">
        <p class="form-control-static">
            {{ __('List mailboxes, search and read conversations, reply, add a note, set the status, assign.') }}
            {{ __('Everything happens as the signed-in user and appears in the conversation history like any other reply.') }}
        </p>
    </div>
</div>
