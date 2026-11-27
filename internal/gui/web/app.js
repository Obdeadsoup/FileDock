(() => {
  const csrfToken = document.querySelector('meta[name="gui-csrf"]')?.content || '';

  const state = {
    loggedIn: false,
    user: null,
    localDir: '',
    localEntries: [],
    selectedLocal: null,

    remoteTab: 'files', // 'files' | 'shares'
    remoteStack: [{ id: '', name: 'Remote Root' }],
    remoteFolders: [],
    remoteFiles: [],
    remoteSearchActive: false,
    selectedRemote: null, // { kind: 'folder'|'file', item, etag? }

    shares: [],
    activeShare: null, // { id, resource_type, resource_name, owner_username }
    shareStack: [],
    shareFolders: [],
    shareFiles: [],
    selectedSharedFile: null,

    activeJobId: null,
    pollTimer: null,
  };

  const $ = (id) => document.getElementById(id);

  function showBanner(msg, type = 'info') {
    const el = $('banner');
    el.textContent = msg;
    el.className = `banner ${type}`;
  }

  async function api(method, path, body) {
    const opts = {
      method,
      credentials: 'same-origin',
      headers: { 'X-FileDock-GUI-Token': csrfToken },
    };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const resp = await fetch(path, opts);
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      const err = new Error(data.error || `HTTP ${resp.status}`);
      err.status = resp.status;
      err.data = data;
      throw err;
    }
    return data;
  }

  function formatBytes(n) {
    if (n === undefined || n === null) return '-';
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
    return `${(n / (1024 * 1024)).toFixed(2)} MiB`;
  }

  function formatTime(iso) {
    if (!iso) return '-';
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
  }

  function currentRemoteParentId() {
    return state.remoteStack[state.remoteStack.length - 1]?.id || '';
  }

  function currentShareParentId() {
    if (!state.shareStack.length) return '';
    return state.shareStack[state.shareStack.length - 1]?.id || '';
  }

  // --- Modals ---
  function openConfirm(title, message) {
    return new Promise((resolve) => {
      $('confirm-title').textContent = title;
      $('confirm-message').textContent = message;
      $('confirm-modal').classList.remove('hidden');
      const cleanup = (val) => {
        $('confirm-modal').classList.add('hidden');
        $('btn-confirm-ok').onclick = null;
        $('btn-confirm-cancel').onclick = null;
        resolve(val);
      };
      $('btn-confirm-ok').onclick = () => cleanup(true);
      $('btn-confirm-cancel').onclick = () => cleanup(false);
    });
  }

  function openConflict(targetPath, suggestedName) {
    return new Promise((resolve) => {
      $('conflict-message').textContent = `Local file "${targetPath}" already exists. Choose whether to overwrite it or save as "${suggestedName}".`;
      $('btn-conflict-rename').textContent = `Save as "${suggestedName}"`;
      $('conflict-modal').classList.remove('hidden');
      const cleanup = (policy) => {
        $('conflict-modal').classList.add('hidden');
        $('btn-conflict-overwrite').onclick = null;
        $('btn-conflict-rename').onclick = null;
        $('btn-conflict-cancel').onclick = null;
        resolve(policy);
      };
      $('btn-conflict-overwrite').onclick = () => cleanup('overwrite');
      $('btn-conflict-rename').onclick = () => cleanup('rename');
      $('btn-conflict-cancel').onclick = () => cleanup('cancel');
    });
  }

  function openInputModal(title, fields) {
    return new Promise((resolve) => {
      $('input-modal-title').textContent = title;
      const box = $('input-modal-fields');
      box.innerHTML = '';
      const inputs = {};
      for (const f of fields) {
        const lbl = document.createElement('label');
        lbl.textContent = f.label;
        const inp = document.createElement('input');
        inp.type = f.type || 'text';
        inp.value = f.value || '';
        inp.placeholder = f.placeholder || '';
        inp.id = `modal-field-${f.key}`;
        lbl.appendChild(inp);
        box.appendChild(lbl);
        inputs[f.key] = inp;
      }
      $('input-modal').classList.remove('hidden');
      const cleanup = (ok) => {
        $('input-modal').classList.add('hidden');
        $('btn-input-ok').onclick = null;
        $('btn-input-cancel').onclick = null;
        if (!ok) return resolve(null);
        const vals = {};
        for (const k of Object.keys(inputs)) vals[k] = inputs[k].value;
        resolve(vals);
      };
      $('btn-input-ok').onclick = () => cleanup(true);
      $('btn-input-cancel').onclick = () => cleanup(false);
    });
  }

  // --- Auth & Bootstrap ---
  async function refreshStatus() {
    const st = await api('GET', '/api/gui/status');
    $('server-badge').textContent = st.server_url;
    $('local-root-hint').textContent = st.local_dir;
    state.loggedIn = !!st.logged_in;
    state.user = st.user || null;
    if (state.loggedIn && state.user) {
      $('auth-guest').classList.add('hidden');
      $('auth-user').classList.remove('hidden');
      $('current-user-label').textContent = `${state.user.username} (${state.user.id.slice(0, 8)})`;
    } else {
      $('auth-guest').classList.remove('hidden');
      $('auth-user').classList.add('hidden');
      $('current-user-label').textContent = '';
    }
  }

  // --- Local Pane ---
  async function loadLocal(dirPath = state.localDir) {
    try {
      const data = await api('GET', `/api/gui/local/list?path=${encodeURIComponent(dirPath)}`);
      state.localDir = data.path || '';
      state.localEntries = data.entries || [];
      state.selectedLocal = null;
      renderLocal();
    } catch (e) {
      showBanner(`Local list error: ${e.message}`, 'error');
    }
  }

  function renderLocal() {
    const nav = $('local-breadcrumbs');
    nav.innerHTML = '';
    const addCrumb = (label, target) => {
      const btn = document.createElement('button');
      btn.className = 'crumb';
      btn.textContent = label;
      btn.onclick = () => loadLocal(target);
      nav.appendChild(btn);
    };
    addCrumb('Local Root', '');
    if (state.localDir) {
      const parts = state.localDir.split('/');
      let acc = '';
      for (const p of parts) {
        acc = acc ? `${acc}/${p}` : p;
        const sep = document.createElement('span');
        sep.className = 'crumb-sep';
        sep.textContent = '/';
        nav.appendChild(sep);
        addCrumb(p, acc);
      }
    }

    const tbody = $('local-tbody');
    tbody.innerHTML = '';
    if (!state.localEntries.length) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td colspan="4" class="muted">Empty directory</td>';
      tbody.appendChild(tr);
      return;
    }
    for (const entry of state.localEntries) {
      const tr = document.createElement('tr');
      tr.dataset.path = entry.path;
      if (state.selectedLocal && state.selectedLocal.path === entry.path) {
        tr.classList.add('selected');
      }
      tr.onclick = () => {
        state.selectedLocal = entry;
        renderLocal();
      };
      tr.ondblclick = () => {
        if (entry.is_dir) loadLocal(entry.path);
      };
      const nameTd = document.createElement('td');
      nameTd.className = entry.is_dir ? 'entry-dir' : 'entry-name';
      nameTd.textContent = entry.is_dir ? `📁 ${entry.name}` : `📄 ${entry.name}`;
      if (entry.is_dir) {
        const openBtn = document.createElement('button');
        openBtn.className = 'btn sm';
        openBtn.style.marginLeft = '8px';
        openBtn.textContent = 'Open';
        openBtn.onclick = (ev) => { ev.stopPropagation(); loadLocal(entry.path); };
        nameTd.appendChild(openBtn);
      }
      const sizeTd = document.createElement('td');
      sizeTd.textContent = entry.is_dir ? '-' : formatBytes(entry.size);
      const timeTd = document.createElement('td');
      timeTd.textContent = formatTime(entry.mod_time);
      const permTd = document.createElement('td');
      permTd.textContent = entry.permissions;
      tr.append(nameTd, sizeTd, timeTd, permTd);
      tbody.appendChild(tr);
    }
  }

  // --- Remote Pane ---
  async function loadRemote() {
    if (!state.loggedIn) {
      state.remoteFolders = [];
      state.remoteFiles = [];
      state.shares = [];
      renderRemote();
      return;
    }
    try {
      if (state.remoteTab === 'files') {
        const pid = currentRemoteParentId();
        const data = await api('GET', `/api/gui/remote/list?parent_id=${encodeURIComponent(pid)}`);
        state.remoteFolders = data.folders || [];
        state.remoteFiles = data.files || [];
        state.remoteSearchActive = false;
        state.selectedRemote = null;
      } else if (state.activeShare) {
        const pid = currentShareParentId();
        const data = await api(
          'GET',
          `/api/gui/remote/shares/${encodeURIComponent(state.activeShare.id)}/browse?parent_id=${encodeURIComponent(pid)}&resource_type=${encodeURIComponent(state.activeShare.resource_type)}`
        );
        state.shareFolders = data.folders || [];
        state.shareFiles = data.files || [];
        state.selectedSharedFile = null;
      } else {
        const data = await api('GET', '/api/gui/remote/shares?direction=received');
        state.shares = data.shares || [];
        state.selectedSharedFile = null;
      }
      renderRemote();
    } catch (e) {
      showBanner(`Remote error: ${e.message}`, 'error');
    }
  }

  function renderRemote() {
    $('tab-remote-files').classList.toggle('active', state.remoteTab === 'files');
    $('tab-remote-shares').classList.toggle('active', state.remoteTab === 'shares');
    $('remote-search-bar').classList.toggle('hidden', state.remoteTab !== 'files');
    $('remote-toolbar').classList.toggle('hidden', state.remoteTab !== 'files');
    $('shares-toolbar').classList.toggle('hidden', state.remoteTab !== 'shares');
    $('btn-shares-back').classList.toggle('hidden', !state.activeShare);

    const nav = $('remote-breadcrumbs');
    nav.innerHTML = '';
    const tbody = $('remote-tbody');
    tbody.innerHTML = '';

    if (!state.loggedIn) {
      tbody.innerHTML = '<tr><td colspan="4" class="muted">Please login to browse remote files</td></tr>';
      return;
    }

    if (state.remoteTab === 'files') {
      state.remoteStack.forEach((crumb, idx) => {
        if (idx > 0) {
          const sep = document.createElement('span');
          sep.className = 'crumb-sep';
          sep.textContent = '/';
          nav.appendChild(sep);
        }
        const btn = document.createElement('button');
        btn.className = 'crumb';
        btn.textContent = crumb.name;
        btn.onclick = () => {
          state.remoteStack = state.remoteStack.slice(0, idx + 1);
          loadRemote();
        };
        nav.appendChild(btn);
      });
      if (state.remoteSearchActive) {
        const tag = document.createElement('span');
        tag.className = 'muted';
        tag.style.marginLeft = '8px';
        tag.textContent = '(Search results)';
        nav.appendChild(tag);
      }

      if (!state.remoteFolders.length && !state.remoteFiles.length) {
        tbody.innerHTML = '<tr><td colspan="4" class="muted">No items</td></tr>';
        return;
      }
      for (const folder of state.remoteFolders) {
        const tr = document.createElement('tr');
        tr.dataset.id = folder.id;
        if (state.selectedRemote?.kind === 'folder' && state.selectedRemote.item.id === folder.id) {
          tr.classList.add('selected');
        }
        tr.onclick = () => {
          state.selectedRemote = { kind: 'folder', item: folder };
          renderRemote();
        };
        tr.ondblclick = () => {
          state.remoteStack.push({ id: folder.id, name: folder.name });
          loadRemote();
        };
        const nameTd = document.createElement('td');
        nameTd.className = 'entry-dir';
        nameTd.textContent = `📁 ${folder.name}`;
        const openBtn = document.createElement('button');
        openBtn.className = 'btn sm';
        openBtn.style.marginLeft = '8px';
        openBtn.textContent = 'Open';
        openBtn.onclick = (ev) => {
          ev.stopPropagation();
          state.remoteStack.push({ id: folder.id, name: folder.name });
          loadRemote();
        };
        nameTd.appendChild(openBtn);
        tr.append(nameTd, cell('-'), cell(formatTime(folder.updated_at || folder.created_at)), cell('read/write'));
        tbody.appendChild(tr);
      }
      for (const file of state.remoteFiles) {
        const tr = document.createElement('tr');
        tr.dataset.id = file.id;
        if (state.selectedRemote?.kind === 'file' && state.selectedRemote.item.id === file.id) {
          tr.classList.add('selected');
        }
        tr.onclick = async () => {
          state.selectedRemote = { kind: 'file', item: file, etag: '' };
          renderRemote();
          try {
            const head = await api('GET', `/api/gui/remote/files/${encodeURIComponent(file.id)}/head`);
            if (state.selectedRemote?.kind === 'file' && state.selectedRemote.item.id === file.id) {
              state.selectedRemote.etag = head.etag || '';
            }
          } catch (_) {}
        };
        const nameTd = document.createElement('td');
        nameTd.className = 'entry-name';
        nameTd.textContent = `📄 ${file.name}`;
        tr.append(
          nameTd,
          cell(formatBytes(file.size)),
          cell(formatTime(file.created_at)),
          cell(`read/write · v${file.content_revision || 1}`),
        );
        tbody.appendChild(tr);
      }
      return;
    }

    // Received shares view
    if (!state.activeShare) {
      const span = document.createElement('span');
      span.className = 'muted';
      span.textContent = 'Received Shares (Read-Only)';
      nav.appendChild(span);
      if (!state.shares.length) {
        tbody.innerHTML = '<tr><td colspan="4" class="muted">No received shares</td></tr>';
        return;
      }
      for (const sh of state.shares) {
        const tr = document.createElement('tr');
        tr.dataset.shareId = sh.id;
        const nameTd = document.createElement('td');
        nameTd.className = sh.resource_type === 'folder' ? 'entry-dir' : 'entry-name';
        nameTd.textContent = `${sh.resource_type === 'folder' ? '📁' : '📄'} ${sh.resource_name}`;
        const openBtn = document.createElement('button');
        openBtn.className = 'btn sm';
        openBtn.style.marginLeft = '8px';
        openBtn.textContent = 'Open';
        openBtn.onclick = (ev) => {
          ev.stopPropagation();
          state.activeShare = sh;
          state.shareStack = [{ id: '', name: sh.resource_name }];
          loadRemote();
        };
        nameTd.appendChild(openBtn);
        tr.append(
          nameTd,
          cell(sh.resource_type),
          cell(formatTime(sh.created_at)),
          cell(`read-only · from ${sh.owner_username}`),
        );
        tbody.appendChild(tr);
      }
      return;
    }

    // Inside a specific share
    state.shareStack.forEach((crumb, idx) => {
      if (idx > 0) {
        const sep = document.createElement('span');
        sep.className = 'crumb-sep';
        sep.textContent = '/';
        nav.appendChild(sep);
      }
      const btn = document.createElement('button');
      btn.className = 'crumb';
      btn.textContent = crumb.name;
      btn.onclick = () => {
        state.shareStack = state.shareStack.slice(0, idx + 1);
        loadRemote();
      };
      nav.appendChild(btn);
    });
    if (!state.shareFolders.length && !state.shareFiles.length) {
      tbody.innerHTML = '<tr><td colspan="4" class="muted">Empty shared view</td></tr>';
      return;
    }
    for (const folder of state.shareFolders) {
      const tr = document.createElement('tr');
      const nameTd = document.createElement('td');
      nameTd.className = 'entry-dir';
      nameTd.textContent = `📁 ${folder.name}`;
      const openBtn = document.createElement('button');
      openBtn.className = 'btn sm';
      openBtn.style.marginLeft = '8px';
      openBtn.textContent = 'Open';
      openBtn.onclick = (ev) => {
        ev.stopPropagation();
        state.shareStack.push({ id: folder.id, name: folder.name });
        loadRemote();
      };
      nameTd.appendChild(openBtn);
      tr.append(nameTd, cell('-'), cell(formatTime(folder.updated_at)), cell('read-only'));
      tbody.appendChild(tr);
    }
    for (const file of state.shareFiles) {
      const tr = document.createElement('tr');
      tr.dataset.id = file.id;
      if (state.selectedSharedFile?.id === file.id) tr.classList.add('selected');
      tr.onclick = () => {
        state.selectedSharedFile = file;
        renderRemote();
      };
      const nameTd = document.createElement('td');
      nameTd.className = 'entry-name';
      nameTd.textContent = `📄 ${file.name}`;
      tr.append(nameTd, cell(formatBytes(file.size)), cell(formatTime(file.created_at)), cell('read-only'));
      tbody.appendChild(tr);
    }
  }

  function cell(text) {
    const td = document.createElement('td');
    td.textContent = text;
    return td;
  }

  // --- Resumable Upload Polling ---
  function renderJob(job) {
    const pct = job.total_chunks > 0
      ? Math.min(100, Math.round((job.confirmed_chunks / job.total_chunks) * 100))
      : (job.status === 'completed' ? 100 : 0);
    $('job-progress-bar').style.width = `${pct}%`;
    $('resume-session-id').value = job.session_id || '';
    $('resume-local-path').value = job.local_path || '';
    $('btn-job-cancel').classList.toggle('hidden', job.status !== 'running');

    let summary = `session=${job.session_id} · status=${job.status} · chunks=${job.confirmed_chunks}/${job.total_chunks} (sent=${job.chunks_sent}, skipped=${job.chunks_skipped}) · bytes_sent=${job.bytes_sent}`;
    if (job.file_id) summary += ` · file_id=${job.file_id}`;
    if (job.error) summary += ` · ${job.error}`;
    $('job-status-text').textContent = summary;
  }

  async function pollJob(jobId) {
    if (state.pollTimer) clearInterval(state.pollTimer);
    state.activeJobId = jobId;
    const tick = async () => {
      try {
        const job = await api('GET', `/api/gui/transfer/jobs/${encodeURIComponent(jobId)}`);
        renderJob(job);
        if (job.status !== 'running') {
          clearInterval(state.pollTimer);
          state.pollTimer = null;
          if (job.status === 'completed') {
            showBanner(`Chunked upload completed: ${job.file_name} (bytes_sent=${job.bytes_sent}, chunks_sent=${job.chunks_sent}/${job.total_chunks})`, 'success');
            loadRemote();
          } else if (job.status === 'canceled') {
            showBanner(`Chunked upload paused/interrupted. Session ID: ${job.session_id}`, 'info');
          } else {
            showBanner(`Chunked upload failed: ${job.error}`, 'error');
          }
        }
      } catch (e) {
        clearInterval(state.pollTimer);
        state.pollTimer = null;
      }
    };
    await tick();
    state.pollTimer = setInterval(tick, 300);
  }

  // --- Download Helper with Same-Name Conflict Prompt ---
  async function performDownload(endpoint, payload) {
    try {
      const res = await api('POST', endpoint, { ...payload, conflict_policy: 'ask' });
      showBanner(`Downloaded to ${res.saved_path} (SHA-256 verified: ${res.sha256.slice(0, 12)}...)`, 'success');
      loadLocal();
    } catch (e) {
      if (e.status === 409 && e.data?.conflict) {
        const policy = await openConflict(e.data.target_path, e.data.suggested_name);
        if (policy === 'cancel') return;
        try {
          const res = await api('POST', endpoint, { ...payload, conflict_policy: policy });
          showBanner(`Downloaded to ${res.saved_path} (SHA-256 verified: ${res.sha256.slice(0, 12)}...)`, 'success');
          loadLocal();
        } catch (err2) {
          showBanner(`Download failed: ${err2.message}`, 'error');
        }
        return;
      }
      showBanner(`Download failed: ${e.message}`, 'error');
    }
  }

  // --- Event Bindings ---
  $('btn-register').onclick = async () => {
    try {
      await api('POST', '/api/gui/auth/register', {
        username: $('auth-username').value.trim(),
        password: $('auth-password').value,
      });
      showBanner('Account registered. You can now log in.', 'success');
    } catch (e) {
      showBanner(`Register failed: ${e.message}`, 'error');
    }
  };

  $('btn-login').onclick = async () => {
    try {
      await api('POST', '/api/gui/auth/login', {
        username: $('auth-username').value.trim(),
        password: $('auth-password').value,
      });
      $('auth-password').value = '';
      state.remoteStack = [{ id: '', name: 'Remote Root' }];
      state.activeShare = null;
      await refreshStatus();
      await loadRemote();
      showBanner(`Logged in as ${state.user.username}`, 'success');
    } catch (e) {
      showBanner(`Login failed: ${e.message}`, 'error');
    }
  };

  $('btn-logout').onclick = async () => {
    try {
      await api('POST', '/api/gui/auth/logout');
      await refreshStatus();
      state.remoteStack = [{ id: '', name: 'Remote Root' }];
      state.activeShare = null;
      await loadRemote();
      showBanner('Logged out', 'info');
    } catch (e) {
      showBanner(`Logout failed: ${e.message}`, 'error');
    }
  };

  $('btn-local-refresh').onclick = () => loadLocal();

  $('btn-local-mkdir').onclick = async () => {
    const vals = await openInputModal('New Local Folder', [{ key: 'name', label: 'Folder name', placeholder: 'e.g. docs' }]);
    if (!vals || !vals.name.trim()) return;
    try {
      await api('POST', '/api/gui/local/mkdir', { parent: state.localDir, name: vals.name.trim() });
      showBanner(`Created local folder "${vals.name.trim()}"`, 'success');
      loadLocal();
    } catch (e) {
      showBanner(`Local mkdir failed: ${e.message}`, 'error');
    }
  };

  $('btn-local-rename').onclick = async () => {
    if (!state.selectedLocal) return showBanner('Select a local item first', 'error');
    const vals = await openInputModal('Rename / Move Local Item', [
      { key: 'dst_dir', label: 'Destination relative directory (empty = root)', value: state.localDir },
      { key: 'new_name', label: 'New name', value: state.selectedLocal.name },
    ]);
    if (!vals) return;
    try {
      await api('POST', '/api/gui/local/rename', {
        path: state.selectedLocal.path,
        dst_dir: vals.dst_dir.trim(),
        new_name: vals.new_name.trim(),
      });
      showBanner('Local item renamed/moved', 'success');
      loadLocal();
    } catch (e) {
      showBanner(`Local rename/move failed: ${e.message}`, 'error');
    }
  };

  $('btn-local-delete').onclick = async () => {
    if (!state.selectedLocal) return showBanner('Select a local item first', 'error');
    const kind = state.selectedLocal.is_dir ? 'local folder' : 'local file';
    const ok = await openConfirm('Confirm Local Delete', `Delete ${kind} "${state.selectedLocal.path}"?`);
    if (!ok) return;
    try {
      await api('POST', '/api/gui/local/delete', { path: state.selectedLocal.path });
      showBanner(`Deleted local ${state.selectedLocal.path}`, 'success');
      loadLocal();
    } catch (e) {
      showBanner(`Local delete failed: ${e.message}`, 'error');
    }
  };

  $('btn-upload-simple').onclick = async () => {
    if (!state.loggedIn) return showBanner('Please login first', 'error');
    if (!state.selectedLocal || state.selectedLocal.is_dir) return showBanner('Select a local file to upload', 'error');
    try {
      const pid = currentRemoteParentId();
      const uploaded = await api('POST', '/api/gui/transfer/upload', {
        local_path: state.selectedLocal.path,
        parent_id: pid || null,
      });
      showBanner(`Uploaded "${uploaded.name}" (id=${uploaded.id.slice(0, 8)}..., sha256=${uploaded.sha256.slice(0, 12)}...)`, 'success');
      loadRemote();
    } catch (e) {
      showBanner(`Upload failed: ${e.message}`, 'error');
    }
  };

  $('btn-upload-resumable').onclick = async () => {
    if (!state.loggedIn) return showBanner('Please login first', 'error');
    if (!state.selectedLocal || state.selectedLocal.is_dir) return showBanner('Select a local file for chunked upload', 'error');
    try {
      const pid = currentRemoteParentId();
      const job = await api('POST', '/api/gui/transfer/upload-resumable', {
        local_path: state.selectedLocal.path,
        parent_id: pid || null,
      });
      renderJob(job);
      pollJob(job.job_id);
    } catch (e) {
      showBanner(`Chunked upload failed: ${e.message}`, 'error');
    }
  };

  $('btn-job-cancel').onclick = async () => {
    if (!state.activeJobId) return;
    try {
      const job = await api('POST', `/api/gui/transfer/jobs/${encodeURIComponent(state.activeJobId)}/cancel`);
      renderJob(job);
    } catch (e) {
      showBanner(`Cancel failed: ${e.message}`, 'error');
    }
  };

  $('btn-resume-upload').onclick = async () => {
    if (!state.loggedIn) return showBanner('Please login first', 'error');
    const sid = $('resume-session-id').value.trim();
    const lp = $('resume-local-path').value.trim();
    if (!sid || !lp) return showBanner('Enter both Session ID and Local File path to resume', 'error');
    try {
      const job = await api('POST', '/api/gui/transfer/upload-resumable', {
        local_path: lp,
        session_id: sid,
      });
      renderJob(job);
      pollJob(job.job_id);
    } catch (e) {
      showBanner(`Resume upload failed: ${e.message}`, 'error');
    }
  };

  $('btn-replace-remote').onclick = async () => {
    if (!state.loggedIn) return showBanner('Please login first', 'error');
    if (!state.selectedLocal || state.selectedLocal.is_dir) return showBanner('Select a local file on the left pane', 'error');
    if (!state.selectedRemote || state.selectedRemote.kind !== 'file') return showBanner('Select a remote file on the right pane', 'error');
    const etag = state.selectedRemote.etag || '';
    try {
      const res = await api('POST', '/api/gui/transfer/replace', {
        local_path: state.selectedLocal.path,
        file_id: state.selectedRemote.item.id,
        if_match: etag,
      });
      showBanner(`Replaced "${res.file.name}" -> rev v${res.file.content_revision}`, 'success');
      loadRemote();
    } catch (e) {
      if (e.status === 412) {
        showBanner(`Version Conflict (HTTP 412): remote file "${state.selectedRemote.item.name}" was updated by another session. Refresh before retrying.`, 'error');
      } else {
        showBanner(`Replace failed: ${e.message}`, 'error');
      }
    }
  };

  $('tab-remote-files').onclick = () => {
    state.remoteTab = 'files';
    state.activeShare = null;
    loadRemote();
  };
  $('tab-remote-shares').onclick = () => {
    state.remoteTab = 'shares';
    state.activeShare = null;
    loadRemote();
  };
  $('btn-shares-refresh').onclick = () => loadRemote();
  $('btn-shares-back').onclick = () => {
    state.activeShare = null;
    state.shareStack = [];
    loadRemote();
  };

  $('btn-remote-refresh').onclick = () => loadRemote();

  $('btn-remote-search').onclick = async () => {
    if (!state.loggedIn) return showBanner('Please login first', 'error');
    const q = $('search-q').value;
    const min = $('search-min').value.trim();
    const max = $('search-max').value.trim();
    const params = new URLSearchParams();
    if (q) params.set('q', q);
    if (min) params.set('min_size', min);
    if (max) params.set('max_size', max);
    try {
      const data = await api('GET', `/api/gui/remote/search?${params.toString()}`);
      state.remoteFolders = [];
      state.remoteFiles = data.files || [];
      state.remoteSearchActive = true;
      state.selectedRemote = null;
      renderRemote();
    } catch (e) {
      showBanner(`Search failed: ${e.message}`, 'error');
    }
  };

  $('btn-remote-clear-search').onclick = () => {
    $('search-q').value = '';
    $('search-min').value = '';
    $('search-max').value = '';
    loadRemote();
  };

  $('btn-remote-mkdir').onclick = async () => {
    if (!state.loggedIn) return showBanner('Please login first', 'error');
    const vals = await openInputModal('New Remote Folder', [{ key: 'name', label: 'Folder name' }]);
    if (!vals || !vals.name.trim()) return;
    try {
      const pid = currentRemoteParentId();
      await api('POST', '/api/gui/remote/folders', { name: vals.name.trim(), parent_id: pid || null });
      showBanner(`Created remote folder "${vals.name.trim()}"`, 'success');
      loadRemote();
    } catch (e) {
      showBanner(`Create remote folder failed: ${e.message}`, 'error');
    }
  };

  $('btn-remote-rename').onclick = async () => {
    if (!state.selectedRemote) return showBanner('Select a remote file or folder first', 'error');
    const { kind, item } = state.selectedRemote;
    const vals = await openInputModal(`Rename / Move Remote ${kind}`, [
      { key: 'name', label: 'New name', value: item.name },
      { key: 'parent_id', label: 'Target parent folder ID (leave blank for root, or keep current)', value: item.parent_id || '' },
    ]);
    if (!vals) return;
    const body = { name: vals.name.trim(), parent_id: vals.parent_id.trim() || null };
    try {
      const endpoint = kind === 'folder'
        ? `/api/gui/remote/folders/${encodeURIComponent(item.id)}`
        : `/api/gui/remote/files/${encodeURIComponent(item.id)}`;
      await api('PATCH', endpoint, body);
      showBanner(`Updated remote ${kind} "${body.name}"`, 'success');
      loadRemote();
    } catch (e) {
      showBanner(`Update failed: ${e.message}`, 'error');
    }
  };

  $('btn-remote-share').onclick = async () => {
    if (!state.selectedRemote) return showBanner('Select a remote file or folder to share', 'error');
    const { kind, item } = state.selectedRemote;
    const vals = await openInputModal(`Share ${kind} "${item.name}"`, [
      { key: 'recipient', label: 'Recipient username', placeholder: 'e.g. bob' },
    ]);
    if (!vals || !vals.recipient.trim()) return;
    try {
      await api('POST', '/api/gui/remote/shares', {
        resource_type: kind,
        resource_id: item.id,
        recipient_username: vals.recipient.trim(),
      });
      showBanner(`Shared "${item.name}" with ${vals.recipient.trim()} (read-only)`, 'success');
    } catch (e) {
      showBanner(`Share failed: ${e.message}`, 'error');
    }
  };

  $('btn-remote-delete').onclick = async () => {
    if (!state.selectedRemote) return showBanner('Select a remote file or folder first', 'error');
    const { kind, item } = state.selectedRemote;
    const ok = await openConfirm('Confirm Remote Delete', `Delete remote ${kind} "${item.name}" (id=${item.id})?`);
    if (!ok) return;
    try {
      const endpoint = kind === 'folder'
        ? `/api/gui/remote/folders/${encodeURIComponent(item.id)}`
        : `/api/gui/remote/files/${encodeURIComponent(item.id)}`;
      await api('DELETE', endpoint);
      showBanner(`Deleted remote ${kind} "${item.name}"`, 'success');
      loadRemote();
    } catch (e) {
      showBanner(`Delete failed: ${e.message}`, 'error');
    }
  };

  $('btn-download-remote').onclick = async () => {
    if (!state.selectedRemote || state.selectedRemote.kind !== 'file') {
      return showBanner('Select a remote file to download', 'error');
    }
    const f = state.selectedRemote.item;
    await performDownload('/api/gui/transfer/download', {
      file_id: f.id,
      file_name: f.name,
      expected_sha256: f.sha256,
      local_dir: state.localDir,
    });
  };

  $('btn-download-shared').onclick = async () => {
    if (!state.activeShare || !state.selectedSharedFile) {
      return showBanner('Open a share and select a shared file to download', 'error');
    }
    const f = state.selectedSharedFile;
    await performDownload('/api/gui/transfer/download-shared', {
      share_id: state.activeShare.id,
      file_id: f.id,
      file_name: f.name,
      expected_sha256: f.sha256,
      local_dir: state.localDir,
    });
  };

  // Initial load
  (async () => {
    await refreshStatus();
    await loadLocal('');
    await loadRemote();
  })();
})();
