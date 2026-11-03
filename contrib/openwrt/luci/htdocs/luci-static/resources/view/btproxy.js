'use strict';
'require view';
'require form';
'require fs';
'require poll';
'require rpc';
'require uci';
'require ui';

// The only key file this page manages, and the only one its access rules cover. The page can replace or remove the
// key, and see that it exists, but cannot read it.
var KEY_FILE = '/etc/btproxy.key';
var INIT = '/etc/init.d/btproxy';

var callServiceList = rpc.declare({
	object: 'service',
	method: 'list',
	params: ['name'],
	expect: { '': {} }
});

function isRunning(services, name) {
	var instances = (services[name] || {}).instances || {};
	return Object.keys(instances).some(function(i) { return instances[i].running; });
}

function newKey() {
	var bytes = new Uint8Array(32);
	window.crypto.getRandomValues(bytes);
	return btoa(String.fromCharCode.apply(null, bytes));
}

function loadStatus() {
	return Promise.all([
		callServiceList('btproxy'),
		callServiceList('bluetoothd'),
		fs.exec(INIT, ['enabled']),
		fs.stat(KEY_FILE).catch(function() { return null; }),
		fs.exec('/sbin/logread', ['-e', 'btproxy'])
	]).then(function(r) {
		return {
			running: isRunning(r[0], 'btproxy'),
			bluetoothd: isRunning(r[1], 'bluetoothd'),
			enabled: r[2].code === 0,
			keySet: r[3] != null,
			log: (r[4].stdout || '').split('\n').filter(function(l) { return l.indexOf('btproxy[') >= 0; }).slice(-40).join('\n')
		};
	});
}

return view.extend({
	load: function() {
		return Promise.all([uci.load('btproxy'), loadStatus()]);
	},

	// Fails with a visible error; the caller refreshes the status either way.
	service: function(action) {
		return fs.exec(INIT, [action]).then(function(r) {
			if (r.code !== 0)
				throw new Error(r.stderr || _('Exit code %d').format(r.code));
		}).catch(function(e) {
			ui.addNotification(null, E('p', _('Could not run %s: %s').format(action, e.message)), 'error');
		}).then(L.bind(this.refresh, this));
	},

	// The proxy reads its key when it starts. A stopped service stays stopped.
	afterKeyChange: function() {
		return this.state.running ? this.service('restart') : this.refresh();
	},

	generateKey: function() {
		var key = newKey();
		return fs.write(KEY_FILE, key + '\n', 384 /* 0600 */).then(L.bind(function() {
			ui.showModal(_('New encryption key'), [
				E('p', _('Enter this key in Home Assistant, in the ESPHome integration. It is shown only once: this page cannot read it back.')),
				E('pre', { 'style': 'user-select: all' }, key),
				E('div', { 'class': 'right' }, E('button', { 'class': 'btn', 'click': ui.hideModal }, _('Close')))
			]);
			return this.afterKeyChange();
		}, this)).catch(function(e) {
			ui.addNotification(null, E('p', _('Could not write the key: %s').format(e.message)), 'error');
		});
	},

	removeKey: function() {
		return fs.remove(KEY_FILE).then(L.bind(this.afterKeyChange, this)).catch(function(e) {
			ui.addNotification(null, E('p', _('Could not remove the key: %s').format(e.message)), 'error');
		});
	},

	// Replacing or removing a key disconnects Home Assistant until it has the new setting, so ask first.
	confirmKey: function(title, text, action) {
		ui.showModal(title, [
			E('p', text),
			E('div', { 'class': 'right' }, [
				E('button', { 'class': 'btn', 'click': ui.hideModal }, _('Cancel')),
				' ',
				E('button', {
					'class': 'btn cbi-button-negative',
					'click': ui.createHandlerFn(this, function() {
						ui.hideModal();
						return action.call(this);
					})
				}, _('Continue'))
			])
		]);
	},

	button: function(label, cls, handler) {
		return E('button', {
			'class': 'btn ' + cls,
			'click': ui.createHandlerFn(this, handler)
		}, label);
	},

	renderService: function(s) {
		var buttons = [];
		if (s.running) {
			buttons.push(this.button(_('Restart'), 'cbi-button-action', function() { return this.service('restart'); }), ' ');
			buttons.push(this.button(_('Stop'), 'cbi-button-negative', function() { return this.service('stop'); }), ' ');
		}
		else {
			buttons.push(this.button(_('Start'), 'cbi-button-action', function() { return this.service('start'); }), ' ');
		}
		buttons.push(s.enabled
			? this.button(_('Disable at boot'), 'cbi-button-neutral', function() { return this.service('disable'); })
			: this.button(_('Enable at boot'), 'cbi-button-neutral', function() { return this.service('enable'); }));

		var row = function(label, value) {
			return E('tr', { 'class': 'tr' }, [
				E('td', { 'class': 'td left', 'width': '33%' }, label),
				E('td', { 'class': 'td left' }, value)
			]);
		};
		return [
			E('table', { 'class': 'table' }, [
				row(_('Proxy'), s.running ? _('Running') : _('Stopped')),
				row(_('Start at boot'), s.enabled ? _('Yes') : _('No')),
				row(_('Bluetooth daemon'), s.bluetoothd ? _('Running') : _('Stopped'))
			]),
			E('p', {}, buttons)
		];
	},

	renderKey: function(s) {
		var file = uci.get('btproxy', 'main', 'encryption_key_file');
		if (file && file !== KEY_FILE)
			return E('p', _('The key file is set to %s, which this page does not manage.').format(file));

		var generate = _('Generate key');
		var buttons = s.keySet ? [
			this.button(_('Replace key'), 'cbi-button-action', function() {
				this.confirmKey(_('Replace the key?'), _('Home Assistant disconnects until you enter the new key there.'), this.generateKey);
			}), ' ',
			this.button(_('Remove key'), 'cbi-button-negative', function() {
				this.confirmKey(_('Remove the key?'), _('The API is then unencrypted, and Home Assistant disconnects until you remove the key there too.'), this.removeKey);
			})
		] : [
			this.button(generate, 'cbi-button-action', function() { return this.generateKey(); })
		];
		return [
			E('p', s.keySet ? _('Encryption is on: a key is set.') : _('Encryption is off: anyone on the network can use the proxy.')),
			E('p', {}, buttons)
		];
	},

	refresh: function() {
		return loadStatus().then(L.bind(this.update, this));
	},

	update: function(s) {
		this.state = s;
		L.dom.content(this.nodes.service, this.renderService(s));
		L.dom.content(this.nodes.key, this.renderKey(s));
		// Stays at the newest line, unless the reader scrolled up to read something.
		var log = this.nodes.log;
		var text = s.log || _('No log lines.');
		if (log.textContent !== text) {
			var atEnd = log.scrollHeight - log.scrollTop - log.clientHeight < 8;
			log.textContent = text;
			if (atEnd)
				log.scrollTop = log.scrollHeight;
		}
	},

	render: function(data) {
		var m = new form.Map('btproxy', _('Bluetooth proxy'),
			_('ESPHome-compatible Bluetooth proxy. Home Assistant finds it through the ESPHome integration, on port 6053.'));
		var s = m.section(form.NamedSection, 'main', 'btproxy', _('Settings'));
		var o;

		o = s.option(form.Value, 'name', _('Node name'), _('Empty: btproxy-, followed by the host name.'));
		o.datatype = 'and(maxlength(31),hostname)';
		o.rmempty = true;

		o = s.option(form.Value, 'friendly_name', _('Friendly name'));
		o.rmempty = true;

		o = s.option(form.Value, 'area', _('Area'), _('Suggested area in Home Assistant.'));
		o.rmempty = true;

		o = s.option(form.Value, 'bluetoothd_restart', _('Bluetooth restart command'),
			_('Runs when bluetoothd hangs, and for the restart button in Home Assistant. Empty turns both off.'));
		o.rmempty = true;

		o = s.option(form.Value, 'extra_args', _('More flags'), _('For anything without a field here: see btproxy -h.'));
		o.rmempty = true;

		this.nodes = {
			service: E('div'),
			key: E('div'),
			log: E('pre', { 'style': 'max-height: 24em; overflow: auto; white-space: pre-wrap' })
		};
		this.update(data[1]);

		return m.render().then(L.bind(function(form) {
			poll.add(L.bind(this.refresh, this), 5);
			// The log has no height until the page is in the document.
			window.requestAnimationFrame(L.bind(function() { this.nodes.log.scrollTop = this.nodes.log.scrollHeight; }, this));
			return E('div', {}, [
				form,
				E('div', { 'class': 'cbi-section' }, [E('h3', _('Service')), this.nodes.service]),
				E('div', { 'class': 'cbi-section' }, [E('h3', _('Encryption')), this.nodes.key]),
				E('div', { 'class': 'cbi-section' }, [E('h3', _('Log')), this.nodes.log])
			]);
		}, this));
	}
});
