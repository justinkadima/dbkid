'use strict';

(() => {
  const byId = (id) => document.getElementById(id);
  const bootView = byId('boot-view');
  const connectView = byId('connect-view');
  const appView = byId('app-view');
  const bootMessage = byId('boot-message');
  const retryBootstrap = byId('retry-bootstrap');
  let csrfToken = '';
  let tlsChosenByUser = false;
  let databaseRequest = 0;
  let tableRequest = 0;
  let rowsRequest = 0;
  let structureRequest = 0;
  let indexesRequest = 0;
  let objectsRequest = 0;
  let statusRequest = 0;
  let foreignKeysRequest = 0;
  let usersRequest = 0;
  let queryRequest = 0;
  const state = {
    connected: false,
    generation: null,
    database: null,
    table: null,
    databases: [],
    tables: [],
    databasesLoading: false,
    databaseError: '',
    tablesLoading: false,
    tableError: '',
    view: 'browse',
    page: 1,
    requestedPage: 1,
    pageSize: 50,
    hasNext: false,
    rowsLoading: false,
    queryRunning: false,
    sortColumn: '',
    sortDir: '',
    structureColumns: null,
    lastRows: null,
    lastCellsTruncated: false,
    history: [],
  };

  // Inline-form and dialog visibility (not persisted anywhere).
  let newDatabaseOpen = false;
  let dropDatabaseOpen = false;
  let columnFormMode = null;
  let columnFormOriginal = null;
  let rowEditor = null;
  let rowEditorSerial = 0;
  let objectEditor = null;
  let objectEditorSerial = 0;
  let definitionSerial = 0;
  let objectOperation = false;
  const replacementLocks = new Set();
  let exportScope = null;
  let importState = null;

  const MODAL_IDS = ['row-editor-modal', 'export-modal', 'import-modal', 'definition-modal', 'object-editor-modal', 'table-create-modal',
    'user-create-modal', 'user-password-modal', 'user-grants-modal', 'privilege-modal', 'user-drop-modal'];
  const modalReturnFocus = new Map();

  const COLUMN_TYPES = ['int', 'bigint', 'smallint', 'tinyint', 'mediumint', 'varchar', 'char',
    'text', 'tinytext', 'mediumtext', 'longtext', 'date', 'datetime', 'timestamp', 'time', 'year',
    'decimal', 'float', 'double', 'boolean', 'bool', 'json', 'blob', 'tinyblob', 'mediumblob',
    'longblob', 'binary', 'varbinary'];
  const IMPORT_LIMIT_BYTES = 8 * 1024 * 1024;

  const TAB_VIEWS = [
    { tab: 'tab-browse', view: 'browse', panel: 'browse-panel', enabled: () => state.table !== null },
    { tab: 'tab-structure', view: 'structure', panel: 'structure-panel', enabled: () => state.table !== null },
    { tab: 'tab-sql', view: 'sql', panel: 'sql-panel', enabled: () => state.database !== null },
    { tab: 'tab-objects', view: 'objects', panel: 'objects-panel', enabled: () => state.database !== null },
    { tab: 'tab-search', view: 'search', panel: 'search-panel', enabled: () => state.database !== null },
    { tab: 'tab-status', view: 'status', panel: 'status-panel', enabled: () => state.connected },
    { tab: 'tab-users', view: 'users', panel: 'users-panel', enabled: () => state.connected },
  ];

  function element(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function newRequestID() {
    return Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) => byte.toString(16).padStart(2, '0')).join('');
  }

  function pick(...candidates) {
    for (const candidate of candidates) {
      if (candidate !== undefined && candidate !== null) return String(candidate);
    }
    return '';
  }

  function binaryColumnType(type) {
    return /binary|blob/.test(String(type || '').toLowerCase());
  }

  function stringColumnType(type) {
    return /^(char|varchar|text|tinytext|mediumtext|longtext|enum|set|json)/.test(String(type || '').toLowerCase().trim());
  }

  function isLoopbackHost(host) {
    const value = host.toLowerCase();
    if (value === 'localhost' || value === 'localhost.' || value === '::1' || value === '[::1]') return true;
    const parts = value.split('.');
    return parts.length === 4 && parts[0] === '127' && parts.every((part) =>
      /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255);
  }

  async function request(path, options = {}) {
    let response;
    try {
      response = await fetch(path, {
        method: options.method || 'GET',
        credentials: 'same-origin',
        cache: 'no-store',
        headers: {
          Accept: 'application/json',
          'X-CSRF-Token': csrfToken,
          ...(options.requestID ? { 'X-DBKid-Request-ID': options.requestID } : {}),
          ...(options.body === undefined ? {} : { 'Content-Type': 'application/json' }),
        },
        ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
      });
    } catch {
      throw new Error(options.networkFailureMessage || 'Could not reach DBKid. Check the connection and try again.');
    }
    let data;
    try {
      data = await response.json();
    } catch {
      throw new Error('The server returned an invalid response.');
    }
    if (!response.ok) {
      const error = new Error(data && typeof data.error === 'string' ? data.error : `Request failed (HTTP ${response.status}).`);
      error.data = data;
      error.status = response.status;
      throw error;
    }
    return data;
  }

  // ---------------------------------------------------------------- modals

  function openModal(id) {
    if (byId(id).hidden) {
      const active = document.activeElement;
      if (active && document.body.contains(active)) modalReturnFocus.set(id, active);
    }
    byId(id).hidden = false;
    const input = byId(id).querySelector('input:not([type="file"]), select, textarea');
    const fallback = byId(id).querySelector('button');
    if (input) input.focus();
    else if (fallback) fallback.focus();
  }

  function closeModal(id) {
    if (id === 'definition-modal') ++definitionSerial;
    byId(id).hidden = true;
    const restore = modalReturnFocus.get(id);
    modalReturnFocus.delete(id);
    if (restore && document.body.contains(restore)) restore.focus({ preventScroll: true });
  }

  function closeAllModals() {
    if (objectEditor && objectEditor.original) showBackgroundObjectOutcome(objectEditor, 'Original definition retained.');
    objectEditor = null;
    ++objectEditorSerial;
    ++definitionSerial;
    for (const id of MODAL_IDS) {
      byId(id).hidden = true;
      modalReturnFocus.delete(id);
    }
  }

  function closeRowEditor() {
    closeModal('row-editor-modal');
    rowEditor = null;
  }

  // ---------------------------------------------------------------- workspace

  function updateWorkspaceContext() {
    const hasDatabase = state.database !== null;
    const hasTable = state.table !== null;
    byId('workspace-title').textContent = hasTable ? state.table : hasDatabase ? state.database : 'Select a database';
    byId('workspace-subtitle').textContent = hasTable
      ? `Database: ${state.database}`
      : hasDatabase ? 'Select a table to browse, or open SQL.' : 'Choose a database from the sidebar to get started.';
    let activePanel = false;
    for (const entry of TAB_VIEWS) {
      const enabled = entry.enabled();
      const pressed = enabled && state.view === entry.view;
      byId(entry.tab).disabled = !enabled;
      byId(entry.tab).setAttribute('aria-pressed', String(pressed));
      byId(entry.panel).hidden = !pressed;
      if (pressed) activePanel = true;
    }
    byId('sql-database').textContent = hasDatabase ? state.database : '';
    byId('objects-database').textContent = hasDatabase ? state.database : '';
    byId('search-database').textContent = hasDatabase ? state.database : '';
    byId('workspace-empty').hidden = activePanel;
    byId('workspace-empty').querySelector('.empty-title').textContent = hasDatabase ? 'No table selected' : 'No database selected';
    byId('workspace-message').textContent = hasDatabase
      ? 'Select a table in the sidebar, or use the SQL tab.'
      : 'Choose a database from the sidebar.';
  }

  function hideStructureInlineForms() {
    byId('rename-table-form').hidden = true;
    byId('drop-table-form').hidden = true;
    byId('column-form').hidden = true;
    byId('index-form').hidden = true;
    columnFormMode = null;
    columnFormOriginal = null;
  }

  function resetFilterInputs() {
    for (let index = 0; index < 3; index++) {
      byId(`filter-column-${index}`).value = '';
      byId(`filter-op-${index}`).value = '=';
      const input = byId(`filter-value-${index}`);
      input.value = '';
      input.disabled = false;
    }
  }

  function clearTableContext() {
    state.sortColumn = '';
    state.sortDir = '';
    state.structureColumns = null;
    state.lastRows = null;
    state.lastCellsTruncated = false;
    resetFilterInputs();
    hideStructureInlineForms();
    closeAllModals();
    rowEditor = null;
    exportScope = null;
    importState = null;
    newDatabaseOpen = false;
    dropDatabaseOpen = false;
    byId('browse-result').replaceChildren();
    byId('structure-result').replaceChildren();
    byId('indexes-result').replaceChildren();
    byId('indexes-status').textContent = '';
    byId('structure-status').textContent = '';
    byId('structure-status').classList.remove('is-error');
    byId('row-actions-hint').hidden = true;
  }

  function clearDatabaseContext() {
    clearTableContext();
    byId('search-result').replaceChildren();
    byId('search-status').textContent = '';
    byId('search-status').classList.remove('is-error');
    byId('objects-result').replaceChildren();
    byId('objects-status').textContent = '';
    byId('objects-status').classList.remove('is-error');
    byId('processes-result').replaceChildren();
    byId('variables-result').replaceChildren();
    byId('server-status-result').replaceChildren();
    byId('status-status').textContent = '';
    byId('status-status').classList.remove('is-error');
  }

  function renderSidebar() {
    const list = byId('database-list');
    const focused = document.activeElement;
    const focusDatabase = focused && focused.classList.contains('database-button') ? focused.dataset.database : null;
    const focusTable = focused && focused.classList.contains('table-button') ? focused.dataset.table : null;
    list.replaceChildren();
    if (state.databasesLoading) {
      list.append(element('p', 'sidebar-message', 'Loading databases…'));
    } else if (state.databaseError) {
      list.append(element('p', 'sidebar-message', state.databaseError));
    } else if (!state.databases.length) {
      list.append(element('p', 'sidebar-message', 'No databases available.'));
    } else {
      for (const database of state.databases) {
        const item = element('div', 'database-item');
        const button = element('button', 'database-button');
        button.type = 'button';
        button.dataset.database = database;
        button.title = database;
        button.setAttribute('aria-expanded', String(state.database === database));
        const icon = element('span', 'nav-glyph', '▤');
        icon.setAttribute('aria-hidden', 'true');
        button.append(icon, element('span', 'nav-label', database));
        button.addEventListener('click', () => selectDatabase(database));
        item.append(button);
        if (state.database === database) {
          const tables = element('div', 'table-list');
          if (state.tablesLoading) tables.append(element('p', 'sidebar-message', 'Loading tables…'));
          else if (state.tableError) tables.append(element('p', 'sidebar-message', state.tableError));
          else if (!state.tables.length) tables.append(element('p', 'sidebar-message', 'No tables. You can still use SQL.'));
          else for (const table of state.tables) {
            const tableButton = element('button', 'table-button');
            tableButton.type = 'button';
            tableButton.dataset.table = table.name;
            tableButton.title = table.name;
            if (state.table === table.name) tableButton.setAttribute('aria-current', 'true');
            const glyph = element('span', 'nav-glyph', '▦');
            glyph.setAttribute('aria-hidden', 'true');
            tableButton.append(glyph, element('span', 'nav-label', table.name));
            if (table.type === 'VIEW') tableButton.append(element('span', 'table-type', 'VIEW'));
            tableButton.addEventListener('click', () => selectTable(table.name));
            tables.append(tableButton);
          }
          item.append(tables);
        }
        list.append(item);
      }
    }
    const restoreFocus = focusDatabase !== null
      ? Array.from(list.querySelectorAll('.database-button')).find((button) => button.dataset.database === focusDatabase)
      : focusTable !== null
        ? Array.from(list.querySelectorAll('.table-button')).find((button) => button.dataset.table === focusTable)
        : null;
    if (restoreFocus) restoreFocus.focus({ preventScroll: true });
    renderDatabaseTools();
  }

  // ---------------------------------------------------------- database tools (sidebar)

  let sidebarToolsError = '';

  function renderDatabaseTools() {
    const container = byId('sidebar-tools');
    const active = document.activeElement;
    const restoreId = active && container.contains(active) && active.id ? active.id : null;
    container.replaceChildren();
    if (!state.connected) return;
    container.append(element('div', 'sidebar-tools-heading', 'DATABASE TOOLS'));
    if (!newDatabaseOpen) {
      const newButton = element('button', 'sidebar-button', '+ New database');
      newButton.type = 'button';
      newButton.id = 'new-database-button';
      newButton.addEventListener('click', () => {
        newDatabaseOpen = true;
        sidebarToolsError = '';
        renderDatabaseTools();
        const input = byId('new-database-name');
        if (input) input.focus();
      });
      container.append(newButton);
    } else {
      const form = element('div', 'sidebar-form');
      const label = element('label', '', 'New database name');
      label.setAttribute('for', 'new-database-name');
      const input = element('input');
      input.id = 'new-database-name';
      input.autocomplete = 'off';
      input.spellcheck = false;
      const actions = element('div', 'sidebar-form-actions');
      const create = element('button', 'button button-primary', 'Create');
      create.type = 'button';
      create.id = 'new-database-create';
      create.addEventListener('click', submitCreateDatabase);
      const cancel = element('button', 'button button-secondary', 'Cancel');
      cancel.type = 'button';
      cancel.addEventListener('click', () => { newDatabaseOpen = false; sidebarToolsError = ''; renderDatabaseTools(); });
      actions.append(create, cancel);
      form.append(label, input, actions, element('p', 'sidebar-hint', 'The database is created on the connected server.'));
      container.append(form);
    }
    const newTableButton = element('button', 'sidebar-button', '+ New table');
    newTableButton.type = 'button';
    newTableButton.id = 'new-table-button';
    newTableButton.addEventListener('click', openTableCreate);
    container.append(newTableButton);
    if (state.database !== null) {
      const exportButton = element('button', 'sidebar-button', 'Export database');
      exportButton.type = 'button';
      exportButton.addEventListener('click', () => openExportDialog(null));
      container.append(exportButton);
      if (!dropDatabaseOpen) {
        const dropButton = element('button', 'sidebar-button sidebar-button-danger', 'Drop database');
        dropButton.type = 'button';
        dropButton.addEventListener('click', () => {
          dropDatabaseOpen = true;
          sidebarToolsError = '';
          renderDatabaseTools();
          const input = byId('drop-database-name');
          if (input) input.focus();
        });
        container.append(dropButton);
      } else {
        const form = element('div', 'sidebar-form');
        const label = element('label', '', `Type “${state.database}” to confirm`);
        label.setAttribute('for', 'drop-database-name');
        const input = element('input');
        input.id = 'drop-database-name';
        input.autocomplete = 'off';
        input.spellcheck = false;
        input.addEventListener('input', () => {
          byId('drop-database-confirm').disabled = state.database === null || input.value !== state.database;
        });
        const actions = element('div', 'sidebar-form-actions');
        const drop = element('button', 'button button-primary', 'Drop database');
        drop.type = 'button';
        drop.id = 'drop-database-confirm';
        drop.disabled = true;
        drop.addEventListener('click', submitDropDatabase);
        const cancel = element('button', 'button button-secondary', 'Cancel');
        cancel.type = 'button';
        cancel.addEventListener('click', () => { dropDatabaseOpen = false; sidebarToolsError = ''; renderDatabaseTools(); });
        actions.append(drop, cancel);
        form.append(label, input, actions,
          element('p', 'sidebar-hint', 'This destroys the database and every table in it; it cannot be undone.'));
        container.append(form);
      }
    }
    if (sidebarToolsError) container.append(element('p', 'sidebar-hint', sidebarToolsError));
    if (restoreId) {
      const node = byId(restoreId);
      if (node) node.focus({ preventScroll: true });
    }
  }

  async function submitCreateDatabase() {
    const input = byId('new-database-name');
    const name = input.value.trim();
    if (!name) { input.focus(); return; }
    if (!window.confirm(`Create database ${JSON.stringify(name)} on the connected server?`)) return;
    try {
      const data = await request('/api/database/create', {
        method: 'POST', requestID: newRequestID(), body: { name, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      newDatabaseOpen = false;
      sidebarToolsError = '';
      renderDatabaseTools();
      loadDatabases();
      selectDatabase(name);
    } catch (error) {
      sidebarToolsError = error.message || 'Could not create the database.';
      renderDatabaseTools();
    }
  }

  async function submitDropDatabase() {
    if (state.database === null) return;
    if (byId('drop-database-name').value !== state.database) return;
    const database = state.database;
    if (!window.confirm(`Drop database ${JSON.stringify(database)}? The database, all of its tables and all of their data are destroyed and this cannot be undone.`)) return;
    try {
      const data = await request('/api/database/drop', {
        method: 'POST', requestID: newRequestID(), body: { database, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      dropDatabaseOpen = false;
      sidebarToolsError = '';
      renderDatabaseTools();
      loadDatabases();
    } catch (error) {
      sidebarToolsError = error.message || 'Could not drop the database.';
      renderDatabaseTools();
    }
  }

  // ---------------------------------------------------------- create-table form

  const MAX_FORM_COLUMNS = 64;

  function openTableCreate() {
    if (!state.connected) return;
    const databaseSelect = byId('table-create-database');
    databaseSelect.replaceChildren();
    for (const database of state.databases) {
      const option = element('option', '', database);
      option.value = database;
      databaseSelect.append(option);
    }
    if (state.database !== null && state.databases.includes(state.database)) {
      databaseSelect.value = state.database;
    }
    byId('table-create-name').value = '';
    byId('table-create-engine').value = '';
    byId('table-create-status').textContent = '';
    Array.from(byId('table-create-rows').querySelectorAll('.table-create-row'))
      .forEach((row) => row.remove());
    addTableColumnRow();
    openModal('table-create-modal');
  }

  function addTableColumnRow() {
    const rows = byId('table-create-rows');
    if (rows.querySelectorAll('.table-create-row').length >= MAX_FORM_COLUMNS) {
      byId('table-create-status').textContent = `A table can have at most ${MAX_FORM_COLUMNS} columns here.`;
      return;
    }
    const row = element('div', 'table-create-row');

    const name = element('input', 'tc-name');
    name.type = 'text';
    name.autocomplete = 'off';
    name.spellcheck = false;
    name.maxLength = 64;
    name.setAttribute('aria-label', 'Column name');

    const type = element('select', 'tc-type');
    type.setAttribute('aria-label', 'Column type');
    const placeholder = element('option', '', '— type —');
    placeholder.value = '';
    type.append(placeholder);
    for (const columnType of COLUMN_TYPES) {
      const option = element('option', '', columnType);
      option.value = columnType;
      type.append(option);
    }

    const length = element('input', 'tc-length');
    length.type = 'text';
    length.autocomplete = 'off';
    length.spellcheck = false;
    length.maxLength = 3;
    length.setAttribute('aria-label', 'Column length');

    const decimals = element('input', 'tc-decimals');
    decimals.type = 'text';
    decimals.autocomplete = 'off';
    decimals.spellcheck = false;
    decimals.maxLength = 3;
    decimals.setAttribute('aria-label', 'Column decimals');

    const nullable = formCheckbox('tc-nullable', 'Allow NULL');
    const auto = formCheckbox('tc-auto', 'Auto increment');
    const primary = formCheckbox('tc-primary', 'Primary key');
    const unique = formCheckbox('tc-unique', 'Unique');

    const defaultValue = element('input', 'tc-default');
    defaultValue.type = 'text';
    defaultValue.autocomplete = 'off';
    defaultValue.spellcheck = false;
    defaultValue.maxLength = 128;
    defaultValue.setAttribute('aria-label', 'Column default');

    const defaultNull = formCheckbox('tc-default-null', 'Default NULL');

    const remove = element('button', 'button button-secondary tc-remove', 'Remove');
    remove.type = 'button';
    remove.setAttribute('aria-label', 'Remove column');
    remove.addEventListener('click', () => {
      row.remove();
      refreshTableColumnRowButtons();
    });

    const header = element('div', 'table-column-heading');
    header.append(element('strong', 'table-column-number'), remove);
    const fields = element('div', 'table-column-fields');
    function labeledField(labelText, input, className = '') {
      const label = element('label', `table-column-field ${className}`);
      label.append(element('span', '', labelText), input);
      return label;
    }
    fields.append(labeledField('Name', name, 'tc-field-name'), labeledField('Type', type, 'tc-field-type'),
      labeledField('Length', length), labeledField('Decimals', decimals),
      labeledField('Default value', defaultValue, 'tc-field-default'));
    const options = element('div', 'table-column-options');
    options.append(nullable, auto, primary, unique, defaultNull);
    row.append(header, fields, options);
    rows.append(row);
    refreshTableColumnRowButtons();
    name.focus();
  }

  function formCheckbox(className, labelText) {
    const label = element('label', '');
    const input = element('input', className);
    input.type = 'checkbox';
    label.append(input, element('span', '', labelText));
    return label;
  }

  function refreshTableColumnRowButtons() {
    const rows = Array.from(byId('table-create-rows').querySelectorAll('.table-create-row'));
    rows.forEach((row, index) => {
      row.querySelector('.table-column-number').textContent = `Column ${index + 1}`;
      row.querySelector('.tc-remove').setAttribute('aria-label', `Remove column ${index + 1}`);
      row.querySelector('.tc-remove').disabled = rows.length === 1;
    });
  }

  function collectTableCreateColumns() {
    const status = byId('table-create-status');
    const rows = Array.from(byId('table-create-rows').querySelectorAll('.table-create-row'));
    const columns = [];
    for (let index = 0; index < rows.length; index++) {
      const row = rows[index];
      const label = `Column ${index + 1}`;
      const name = row.querySelector('.tc-name').value.trim();
      const type = row.querySelector('.tc-type').value;
      const length = row.querySelector('.tc-length').value.trim();
      if (!name) {
        status.textContent = `${label}: enter a name.`;
        row.querySelector('.tc-name').focus();
        return null;
      }
      if (!type) {
        status.textContent = `${label}: choose a type.`;
        row.querySelector('.tc-type').focus();
        return null;
      }
      if (['varchar', 'char', 'binary', 'varbinary'].includes(type) && !/^\d{1,3}$/.test(length)) {
        status.textContent = `${label}: this type needs a length (for example 30).`;
        row.querySelector('.tc-length').focus();
        return null;
      }
      const defaultNull = row.querySelector('.tc-default-null').checked;
      columns.push({
        name,
        type,
        length,
        decimals: row.querySelector('.tc-decimals').value.trim(),
        nullable: row.querySelector('.tc-nullable').checked,
        auto_increment: row.querySelector('.tc-auto').checked,
        primary: row.querySelector('.tc-primary').checked,
        unique: row.querySelector('.tc-unique').checked,
        default: defaultNull ? '' : row.querySelector('.tc-default').value,
        default_null: defaultNull,
      });
    }
    if (!columns.length) {
      status.textContent = 'Add at least one column.';
      return null;
    }
    return columns;
  }

  async function submitTableCreate() {
    if (!state.connected) return;
    const status = byId('table-create-status');
    const database = byId('table-create-database').value;
    const name = byId('table-create-name').value.trim();
    const engine = byId('table-create-engine').value;
    if (!database) {
      status.textContent = 'Choose a database.';
      return;
    }
    if (!name) {
      status.textContent = 'Enter a table name.';
      byId('table-create-name').focus();
      return;
    }
    const columns = collectTableCreateColumns();
    if (columns === null) return;
    if (!window.confirm(`Create table ${JSON.stringify(name)} in database ${JSON.stringify(database)}? This modifies the database structure.`)) return;
    status.textContent = 'Creating…';
    try {
      const data = await request('/api/table/create', {
        method: 'POST', requestID: newRequestID(),
        body: { database, name, engine, columns, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      closeModal('table-create-modal');
      await selectDatabase(database);
      selectTable(name);
    } catch (error) {
      status.textContent = `${error.message || 'Could not create the table.'} The outcome may be unknown. Inspect the database before retrying.`;
    }
  }

  // ---------------------------------------------------------------- databases / tables

  async function loadDatabases() {
    const serial = ++databaseRequest;
    state.databasesLoading = true;
    state.databaseError = '';
    renderSidebar();
    try {
      const data = await request('/api/databases');
      if (!state.connected || serial !== databaseRequest) return;
      if (!data || !Array.isArray(data.databases) || !data.databases.every((name) => typeof name === 'string')) {
        throw new Error('The database list was invalid.');
      }
      state.databases = data.databases;
      if (state.database !== null && !state.databases.includes(state.database)) {
        state.database = null;
        state.table = null;
        state.tables = [];
        state.view = 'browse';
        state.rowsLoading = false;
        state.hasNext = false;
        ++tableRequest;
        ++rowsRequest;
        ++structureRequest;
        ++indexesRequest;
        resetQuery();
        clearDatabaseContext();
        updateWorkspaceContext();
      }
    } catch (error) {
      if (!state.connected || serial !== databaseRequest) return;
      state.databaseError = error.message || 'Could not load databases.';
    } finally {
      if (state.connected && serial === databaseRequest) {
        state.databasesLoading = false;
        renderSidebar();
      }
    }
  }

  function resetQuery() {
    ++queryRequest;
    state.queryRunning = false;
    byId('run-query').disabled = false;
    byId('run-query').textContent = 'Run SQL';
    byId('sql-editor').value = '';
    highlightSQL();
    byId('query-status').classList.remove('is-error');
    byId('query-status').textContent = 'No query run yet.';
    byId('query-result').replaceChildren();
    byId('query-result').hidden = true;
  }

  async function selectDatabase(database) {
    const serial = ++tableRequest;
    if (state.database !== database) resetQuery();
    state.database = database;
    state.table = null;
    state.view = 'browse';
    ++rowsRequest;
    ++structureRequest;
    ++indexesRequest;
    state.tables = [];
    state.tablesLoading = true;
    state.tableError = '';
    clearDatabaseContext();
    updateWorkspaceContext();
    renderSidebar();
    try {
      const params = new URLSearchParams({ database });
      const data = await request(`/api/tables?${params}`);
      if (!state.connected || serial !== tableRequest) return;
      if (!data || !Array.isArray(data.tables) || !data.tables.every((table) => table && typeof table.name === 'string')) {
        throw new Error('The table list was invalid.');
      }
      state.tables = data.tables;
    } catch (error) {
      if (!state.connected || serial !== tableRequest) return;
      state.tableError = error.message || 'Could not load tables.';
    } finally {
      if (state.connected && serial === tableRequest) {
        state.tablesLoading = false;
        renderSidebar();
      }
    }
  }

  async function refreshTablesList() {
    const params = new URLSearchParams({ database: state.database });
    const data = await request(`/api/tables?${params}`);
    if (!data || !Array.isArray(data.tables) || !data.tables.every((table) => table && typeof table.name === 'string')) {
      throw new Error('The table list was invalid.');
    }
    state.tables = data.tables;
  }

  function selectTable(table, options = {}) {
    ++structureRequest;
    ++indexesRequest;
    if (!options.keepFilters) {
      state.sortColumn = '';
      state.sortDir = '';
      resetFilterInputs();
    }
    hideStructureInlineForms();
    closeAllModals();
    rowEditor = null;
    exportScope = null;
    importState = null;
    state.table = table;
    state.view = 'browse';
    state.page = 1;
    state.hasNext = false;
    state.structureColumns = null;
    state.lastRows = null;
    state.lastCellsTruncated = false;
    updateWorkspaceContext();
    renderSidebar();
    loadRows(1);
    loadStructure();
  }

  // ---------------------------------------------------------------- shared grid helpers

  function appendValue(cell, value) {
    if (value === null) {
      const marker = element('span', 'null-value', 'NULL');
      marker.setAttribute('aria-label', 'SQL NULL');
      cell.append(marker);
    } else {
      cell.textContent = value;
    }
  }

  function validGrid(data) {
    return data && Array.isArray(data.columns) && data.columns.every((column) => typeof column === 'string') &&
      Array.isArray(data.rows) && data.rows.every((row) => Array.isArray(row) && row.length === data.columns.length &&
        row.every((value) => value === null || typeof value === 'string'));
  }

  function renderGrid(container, columns, rows) {
    container.replaceChildren();
    const table = element('table', 'data-grid');
    const head = element('thead');
    const headRow = element('tr');
    for (const column of columns) {
      const cell = element('th', '', column);
      cell.scope = 'col';
      headRow.append(cell);
    }
    head.append(headRow);
    const body = element('tbody');
    for (const values of rows) {
      const row = element('tr');
      for (const value of values) {
        const cell = element('td');
        if (value === null) {
          const marker = element('span', 'null-value', 'NULL');
          marker.setAttribute('aria-label', 'SQL NULL');
          cell.append(marker);
        } else {
          cell.textContent = value;
        }
        row.append(cell);
      }
      body.append(row);
    }
    table.append(head, body);
    container.append(table);
  }

  function validStructureColumns(data) {
    return data && Array.isArray(data.columns) && data.columns.every((column) => column &&
      typeof column.name === 'string' && typeof column.type === 'string' && typeof column.nullable === 'boolean' &&
      typeof column.key === 'string' && (column.default === null || typeof column.default === 'string') && typeof column.extra === 'string');
  }

  function columnType(name) {
    const column = (state.structureColumns || []).find((entry) => entry.name === name);
    return column ? column.type : '';
  }

  function primaryKeyPairs(values) {
    const columns = state.structureColumns;
    const grid = state.lastRows;
    if (!columns || !grid || !Array.isArray(grid.columns)) return null;
    const keyColumns = columns.filter((column) => column.key === 'PRI');
    if (!keyColumns.length) return null;
    const index = new Map(grid.columns.map((name, position) => [name, position]));
    const pairs = [];
    for (const column of keyColumns) {
      const position = index.get(column.name);
      if (position === undefined || values[position] === null || values[position] === undefined) return null;
      pairs.push([column.name, values[position]]);
    }
    return pairs;
  }

  // ---------------------------------------------------------------- browse

  function updatePagination() {
    byId('prev-page').disabled = state.rowsLoading || state.page <= 1;
    byId('next-page').disabled = state.rowsLoading || !state.hasNext;
    byId('page-size').disabled = state.rowsLoading;
  }

  function activeFilters() {
    const filters = [];
    for (let index = 0; index < 3; index++) {
      const column = byId(`filter-column-${index}`).value;
      const op = byId(`filter-op-${index}`).value;
      if (!column || !op) continue;
      if (op === 'is null' || op === 'is not null') {
        filters.push({ column, op, value: '' });
        continue;
      }
      filters.push({ column, op, value: byId(`filter-value-${index}`).value });
    }
    return filters;
  }

  function populateFilterColumns() {
    const columns = state.structureColumns || [];
    for (let index = 0; index < 3; index++) {
      const select = byId(`filter-column-${index}`);
      const current = select.value;
      select.replaceChildren();
      const noneOption = element('option', '', '— column —');
      noneOption.value = '';
      select.append(noneOption);
      let present = false;
      for (const column of columns) {
        const option = element('option', '', column.name);
        option.value = column.name;
        select.append(option);
        if (column.name === current) present = true;
      }
      select.value = present ? current : '';
    }
  }

  function applySort(column) {
    if (state.rowsLoading || state.table === null) return;
    if (state.sortColumn === column) {
      state.sortDir = state.sortDir === 'asc' ? 'desc' : 'asc';
    } else {
      state.sortColumn = column;
      state.sortDir = 'asc';
    }
    loadRows(1);
  }

  function showBrowseError(error) {
    const status = byId('browse-status');
    status.classList.remove('is-error');
    status.textContent = error.message || 'The request failed.';
    status.classList.add('is-error');
  }

  function imageMagicHex(hex) {
    if (hex.startsWith('89504e47')) return true;
    if (hex.startsWith('ffd8ff')) return true;
    if (hex.startsWith('474946')) return true;
    return hex.startsWith('52494646') && hex.slice(16, 24) === '57454250';
  }

  async function requestBlobURL(column, values) {
    const pairs = primaryKeyPairs(values);
    if (pairs === null) throw new Error('A primary key is required to open binary values.');
    const data = await request('/api/blob/token', {
      method: 'POST',
      requestID: newRequestID(),
      body: { database: state.database, table: state.table, column, key: pairs },
    });
    if (!data || typeof data.url !== 'string' || !data.url) throw new Error('The blob response was invalid.');
    return data.url;
  }

  function addBlobOpenLink(cell, column, values) {
    const link = element('button', 'blob-link', 'open');
    link.type = 'button';
    link.title = 'Open this binary value in a new tab';
    link.addEventListener('click', async () => {
      try {
        const url = await requestBlobURL(column, values);
        window.open(url);
      } catch (error) {
        showBrowseError(error);
      }
    });
    cell.append(link);
  }

  function decorateBinaryCell(cell, column, values, value) {
    if (primaryKeyPairs(values) === null) return;
    // Display values carry the "0x" prefix; strip it before matching magics.
    const hex = String(value).replace(/^0x/i, '').toLowerCase();
    if (value.length <= 2048 && imageMagicHex(hex)) {
      requestBlobURL(column, values).then((url) => {
        const img = element('img', 'blob-thumb');
        img.alt = `Binary preview of column ${column}`;
        img.src = url;
        cell.append(img);
      }).catch(() => addBlobOpenLink(cell, column, values));
    } else {
      addBlobOpenLink(cell, column, values);
    }
  }

  function actionButton(label, handler, disabled, hint) {
    const button = element('button', 'button button-secondary', label);
    button.type = 'button';
    button.disabled = disabled === true;
    if (hint) button.title = hint;
    if (disabled !== true) button.addEventListener('click', handler);
    return button;
  }

  function buildRowActionButtons(values) {
    const columns = state.structureColumns;
    let hint = '';
    let canAddress = false;
    if (!columns) {
      hint = 'Column information is unavailable.';
    } else if (primaryKeyPairs(values) === null) {
      hint = 'No primary key; use SQL.';
    } else {
      canAddress = true;
    }
    const copyDisabled = !canAddress || state.lastCellsTruncated;
    const copyHint = state.lastCellsTruncated
      ? 'Value too large: a cell in this row may have been truncated.'
      : hint;
    return [
      actionButton('Edit', () => openRowEditor('edit', values), !canAddress, hint),
      actionButton('Copy', () => openRowEditor('copy', values), copyDisabled, copyHint),
      actionButton('Delete', () => deleteRow(values), !canAddress, hint),
    ];
  }

  function renderBrowseGrid(data) {
    const container = byId('browse-result');
    container.replaceChildren();
    const table = element('table', 'data-grid');
    const head = element('thead');
    const headRow = element('tr');
    data.columns.forEach((column) => {
      const th = element('th');
      th.scope = 'col';
      const button = element('button', 'sort-column', column);
      button.type = 'button';
      button.setAttribute('aria-label', `Sort by ${column}`);
      if (state.sortColumn === column) {
        th.setAttribute('aria-sort', state.sortDir === 'desc' ? 'descending' : 'ascending');
        button.append(element('span', 'sort-indicator', state.sortDir === 'desc' ? ' ↓' : ' ↑'));
      }
      button.addEventListener('click', () => applySort(column));
      th.append(button);
      headRow.append(th);
    });
    const actionsHead = element('th', '', 'Actions');
    actionsHead.scope = 'col';
    headRow.append(actionsHead);
    head.append(headRow);
    const body = element('tbody');
    for (const values of data.rows) {
      const row = element('tr');
      data.columns.forEach((column, index) => {
        const cell = element('td');
        appendValue(cell, values[index]);
        if (values[index] !== null && binaryColumnType(columnType(column))) {
          decorateBinaryCell(cell, column, values, values[index]);
        }
        row.append(cell);
      });
      const actionsCell = element('td');
      const wrap = element('div', 'row-actions');
      for (const button of buildRowActionButtons(values)) wrap.append(button);
      actionsCell.append(wrap);
      row.append(actionsCell);
      body.append(row);
    }
    table.append(head, body);
    container.append(table);
  }

  function updateRowActionsHint() {
    const hint = byId('row-actions-hint');
    const columns = state.structureColumns;
    if (state.table === null || !columns || columns.some((column) => column.key === 'PRI')) {
      hint.hidden = true;
      return;
    }
    hint.textContent = 'This table has no primary key. Row editing, copying and deleting are unavailable; use SQL instead.';
    hint.hidden = false;
  }

  function refreshBrowseActions() {
    updateRowActionsHint();
    if (state.lastRows && state.table !== null && !byId('browse-panel').hidden) renderBrowseGrid(state.lastRows);
  }

  async function loadRows(page) {
    if (state.table === null || state.database === null) return;
    const serial = ++rowsRequest;
    const database = state.database;
    const table = state.table;
    const pageSize = state.pageSize;
    state.requestedPage = page;
    state.rowsLoading = true;
    state.hasNext = false;
    byId('retry-rows').hidden = true;
    byId('browse-status').classList.remove('is-error');
    byId('browse-status').textContent = 'Loading rows…';
    byId('browse-result').replaceChildren();
    updatePagination();
    try {
      const params = new URLSearchParams({ database, table, page: String(page), page_size: String(pageSize) });
      if (state.sortColumn) {
        params.set('sort', state.sortColumn);
        params.set('dir', state.sortDir === 'desc' ? 'desc' : 'asc');
      }
      const filters = activeFilters();
      if (filters.length) params.set('filter', JSON.stringify(filters));
      const data = await request(`/api/rows?${params}`);
      if (!state.connected || serial !== rowsRequest) return;
      if (!validGrid(data) || !Number.isInteger(data.page) || data.page < 1 || typeof data.has_next !== 'boolean') {
        throw new Error('The row response was invalid.');
      }
      state.page = data.page;
      state.hasNext = data.has_next;
      state.lastRows = data;
      state.lastCellsTruncated = data.cells_truncated === true;
      renderBrowseGrid(data);
      updateRowActionsHint();
      byId('page-label').textContent = `Page ${state.page}`;
      const summary = data.rows.length
        ? `Showing ${data.rows.length} ${data.rows.length === 1 ? 'row' : 'rows'}.`
        : 'No rows on this page.';
      byId('browse-status').textContent = `${summary}${data.cells_truncated === true ? ' Some cell values truncated.' : ''}`;
    } catch (error) {
      if (!state.connected || serial !== rowsRequest) return;
      byId('browse-status').textContent = error.message || 'Could not load rows.';
      byId('browse-status').classList.add('is-error');
      byId('retry-rows').hidden = false;
    } finally {
      if (state.connected && serial === rowsRequest) {
        state.rowsLoading = false;
        updatePagination();
      }
    }
  }

  // ---------------------------------------------------------------- structure tab

  function renderStructureGrid(columns) {
    const container = byId('structure-result');
    container.replaceChildren();
    const table = element('table', 'data-grid');
    const head = element('thead');
    const headRow = element('tr');
    for (const label of ['Column', 'Type', 'Nullable', 'Key', 'Default', 'Extra', 'Actions']) {
      const cell = element('th', '', label);
      cell.scope = 'col';
      headRow.append(cell);
    }
    head.append(headRow);
    const body = element('tbody');
    for (const column of columns) {
      const row = element('tr');
      const nameCell = element('td', '', column.name);
      row.append(nameCell);
      row.append(element('td', '', column.type));
      row.append(element('td', '', column.nullable ? 'Yes' : 'No'));
      row.append(element('td', '', column.key));
      const defaultCell = element('td');
      appendValue(defaultCell, column.default);
      row.append(defaultCell);
      row.append(element('td', '', column.extra));
      const actions = element('td');
      const wrap = element('div', 'row-actions');
      const modify = element('button', 'button button-secondary', 'Modify');
      modify.type = 'button';
      modify.addEventListener('click', () => openColumnForm('modify', column));
      const drop = element('button', 'button button-secondary button-danger', 'Drop');
      drop.type = 'button';
      drop.addEventListener('click', () => dropColumn(column));
      wrap.append(modify, drop);
      actions.append(wrap);
      row.append(actions);
      body.append(row);
    }
    table.append(head, body);
    container.append(table);
  }

  async function loadStructure() {
    if (state.table === null || state.database === null) return;
    const serial = ++structureRequest;
    const database = state.database;
    const table = state.table;
    const status = byId('structure-status');
    status.classList.remove('is-error');
    status.textContent = 'Loading structure…';
    byId('structure-result').replaceChildren();
    try {
      const params = new URLSearchParams({ database, table });
      const data = await request(`/api/structure?${params}`);
      if (!state.connected || serial !== structureRequest) return;
      if (!validStructureColumns(data)) throw new Error('The structure response was invalid.');
      state.structureColumns = data.columns;
      renderStructureGrid(data.columns);
      status.textContent = data.columns.length ? `${data.columns.length} ${data.columns.length === 1 ? 'column' : 'columns'}.` : 'No columns found.';
      populateFilterColumns();
      loadIndexes();
      loadForeignKeys();
      refreshBrowseActions();
    } catch (error) {
      if (!state.connected || serial !== structureRequest) return;
      state.structureColumns = null;
      status.textContent = error.message || 'Could not load structure.';
      status.classList.add('is-error');
      byId('indexes-result').replaceChildren();
      byId('indexes-status').textContent = 'Indexes are unavailable without column information.';
      refreshBrowseActions();
    }
  }

  // ---------------------------------------------------------- indexes

  async function loadIndexes() {
    if (state.table === null || state.database === null) return;
    const serial = ++indexesRequest;
    const database = state.database;
    const table = state.table;
    const status = byId('indexes-status');
    status.textContent = 'Loading indexes…';
    try {
      const params = new URLSearchParams({ database, table });
      const data = await request(`/api/indexes?${params}`);
      if (!state.connected || serial !== indexesRequest) return;
      if (!data || !Array.isArray(data.indexes) || !data.indexes.every((index) => index &&
        typeof index.name === 'string' && typeof index.unique === 'boolean' &&
        Array.isArray(index.columns) && index.columns.every((name) => typeof name === 'string') &&
        typeof index.type === 'string')) {
        throw new Error('The index list was invalid.');
      }
      renderIndexes(data.indexes);
      status.textContent = data.indexes.length
        ? `${data.indexes.length} ${data.indexes.length === 1 ? 'index' : 'indexes'}.`
        : 'No indexes on this table.';
    } catch (error) {
      if (!state.connected || serial !== indexesRequest) return;
      byId('indexes-result').replaceChildren();
      status.textContent = error.message || 'Could not load indexes.';
    }
  }

  function renderIndexes(indexes) {
    const container = byId('indexes-result');
    container.replaceChildren();
    if (!indexes.length) {
      container.append(element('p', 'hint-text', 'No indexes on this table.'));
      return;
    }
    const list = element('div', 'index-list');
    for (const index of indexes) {
      const item = element('div', 'index-item');
      item.append(element('span', 'index-name', index.name));
      item.append(element('span', index.unique ? 'badge' : 'badge badge-muted', index.unique ? 'unique' : 'index'));
      if (index.type) item.append(element('span', 'badge badge-muted', index.type));
      item.append(element('span', 'index-columns', index.columns.join(', ')));
      const drop = element('button', 'button button-secondary button-danger', 'Drop');
      drop.type = 'button';
      drop.addEventListener('click', () => dropIndex(index.name));
      item.append(drop);
      list.append(item);
    }
    container.append(list);
  }

  function openIndexForm() {
    if (state.table === null) return;
    byId('rename-table-form').hidden = true;
    byId('drop-table-form').hidden = true;
    byId('column-form').hidden = true;
    const form = byId('index-form');
    form.hidden = false;
    byId('index-form-name').value = '';
    byId('index-form-unique').checked = false;
    byId('index-form-status').textContent = '';
    const columns = byId('index-form-columns');
    columns.replaceChildren();
    for (const column of state.structureColumns || []) {
      const label = element('label', 'check-label');
      const checkbox = document.createElement('input');
      checkbox.type = 'checkbox';
      checkbox.value = column.name;
      label.append(checkbox, element('span', '', column.name));
      columns.append(label);
    }
    byId('index-form-name').focus();
  }

  async function submitIndexForm() {
    if (state.table === null || state.database === null) return;
    const name = byId('index-form-name').value.trim();
    const unique = byId('index-form-unique').checked;
    const checked = Array.from(byId('index-form-columns').querySelectorAll('input[type="checkbox"]:checked'))
      .map((box) => box.value);
    const status = byId('index-form-status');
    if (!name) { status.textContent = 'Enter an index name.'; byId('index-form-name').focus(); return; }
    if (!checked.length) { status.textContent = 'Choose at least one column.'; return; }
    const database = state.database;
    const table = state.table;
    if (!window.confirm(`Create ${unique ? 'unique ' : ''}index ${JSON.stringify(name)} on table ${JSON.stringify(table)} in database ${JSON.stringify(database)}?`)) return;
    status.textContent = 'Creating…';
    try {
      const data = await request('/api/index/create', {
        method: 'POST', requestID: newRequestID(),
        body: { database, table, name, columns: checked, unique, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      byId('index-form').hidden = true;
      loadIndexes();
      loadStructure();
    } catch (error) {
      status.textContent = error.message || 'Could not create the index.';
    }
  }

  async function dropIndex(name) {
    if (state.table === null || state.database === null) return;
    const database = state.database;
    const table = state.table;
    if (!window.confirm(`Drop index ${JSON.stringify(name)} from table ${JSON.stringify(table)} in database ${JSON.stringify(database)}? This cannot be undone.`)) return;
    const status = byId('indexes-status');
    status.textContent = 'Dropping index…';
    try {
      const data = await request('/api/index/drop', {
        method: 'POST', requestID: newRequestID(),
        body: { database, table, name, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      loadIndexes();
      loadStructure();
    } catch (error) {
      status.textContent = error.message || 'Could not drop the index.';
    }
  }

  // ---------------------------------------------------------------- foreign keys

  async function loadForeignKeys() {
    if (state.table === null || state.database === null) return;
    const serial = ++foreignKeysRequest;
    const database = state.database;
    const table = state.table;
    const status = byId('foreign-keys-status');
    status.textContent = 'Loading foreign keys…';
    try {
      const params = new URLSearchParams({ database, table });
      const data = await request(`/api/foreign-keys?${params}`);
      if (!state.connected || serial !== foreignKeysRequest) return;
      if (!data || !Array.isArray(data.foreign_keys) || !data.foreign_keys.every((fk) => fk && typeof fk.name === 'string' &&
        Array.isArray(fk.columns) && fk.columns.every((name) => typeof name === 'string') &&
        typeof fk.referenced_database === 'string' && typeof fk.referenced_table === 'string' &&
        Array.isArray(fk.referenced_columns) && fk.referenced_columns.every((name) => typeof name === 'string') &&
        typeof fk.on_delete === 'string' && typeof fk.on_update === 'string')) {
        throw new Error('The foreign key list was invalid.');
      }
      renderForeignKeys(data.foreign_keys);
      status.textContent = data.foreign_keys.length
        ? `${data.foreign_keys.length} foreign ${data.foreign_keys.length === 1 ? 'key' : 'keys'}.`
        : 'No foreign keys on this table.';
    } catch (error) {
      if (!state.connected || serial !== foreignKeysRequest) return;
      byId('foreign-keys-result').replaceChildren();
      status.textContent = error.message || 'Could not load foreign keys.';
    }
  }

  function renderForeignKeys(foreignKeys) {
    const container = byId('foreign-keys-result');
    container.replaceChildren();
    if (!foreignKeys.length) {
      container.append(element('p', 'hint-text', 'No foreign keys on this table.'));
      return;
    }
    const list = element('div', 'fk-list');
    for (const fk of foreignKeys) {
      const item = element('div', 'fk-item');
      item.append(element('span', 'fk-name', fk.name));
      item.append(element('span', 'fk-mapping',
        `${fk.columns.join(', ')} → ${fk.referenced_database}.${fk.referenced_table} (${fk.referenced_columns.join(', ')})`));
      item.append(element('span', 'fk-rule', `ON DELETE ${fk.on_delete} · ON UPDATE ${fk.on_update}`));
      const drop = element('button', 'button button-secondary', 'Drop');
      drop.type = 'button';
      drop.addEventListener('click', () => dropForeignKey(fk.name));
      item.append(drop);
      list.append(item);
    }
    container.append(list);
  }

  async function dropForeignKey(name) {
    if (state.table === null || state.database === null) return;
    const database = state.database;
    const table = state.table;
    const status = byId('foreign-keys-status');
    if (!window.confirm(`Drop foreign key ${JSON.stringify(name)} from table ${JSON.stringify(table)} in database ${JSON.stringify(database)}? This cannot be undone.`)) return;
    try {
      const data = await request('/api/foreign-key/drop', {
        method: 'POST', requestID: newRequestID(),
        body: { database, table, name, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      loadForeignKeys();
      loadIndexes();
    } catch (error) {
      status.textContent = error.message || 'Could not drop the foreign key.';
    }
  }

  function openForeignKeyForm() {
    if (state.table === null || state.database === null || state.structureColumns === null) return;
    byId('fk-form-scope').textContent = `Table ${JSON.stringify(state.table)} in database ${JSON.stringify(state.database)}.`;
    byId('fk-name').value = '';
    byId('fk-on-delete').value = '';
    byId('fk-on-update').value = '';
    const columnSelect = byId('fk-column-0');
    columnSelect.replaceChildren();
    for (const column of state.structureColumns) {
      const option = element('option', '', column.name);
      option.value = column.name;
      columnSelect.append(option);
    }
    const databaseSelect = byId('fk-ref-db-0');
    databaseSelect.replaceChildren();
    for (const name of state.databases) {
      const option = element('option', '', name);
      option.value = name;
      if (name === state.database) option.selected = true;
      databaseSelect.append(option);
    }
    byId('fk-ref-table-0').replaceChildren();
    byId('fk-ref-column-0').replaceChildren();
    byId('fk-form-status').textContent = '';
    byId('fk-form').hidden = false;
    loadForeignKeyRefTables();
  }

  async function loadForeignKeyRefTables() {
    const database = byId('fk-ref-db-0').value;
    const tableSelect = byId('fk-ref-table-0');
    tableSelect.replaceChildren();
    byId('fk-ref-column-0').replaceChildren();
    if (!database) return;
    try {
      const params = new URLSearchParams({ database });
      const data = await request(`/api/tables?${params}`);
      if (!data || !Array.isArray(data.tables)) throw new Error('The table list was invalid.');
      for (const table of data.tables) {
        if (table.type !== 'BASE TABLE') continue;
        const option = element('option', '', table.name);
        option.value = table.name;
        tableSelect.append(option);
      }
      loadForeignKeyRefColumns();
    } catch (error) {
      byId('fk-form-status').textContent = error.message || 'Could not list the tables of that database.';
    }
  }

  async function loadForeignKeyRefColumns() {
    const database = byId('fk-ref-db-0').value;
    const table = byId('fk-ref-table-0').value;
    const columnSelect = byId('fk-ref-column-0');
    columnSelect.replaceChildren();
    if (!database || !table) return;
    try {
      const params = new URLSearchParams({ database, table });
      const data = await request(`/api/structure?${params}`);
      if (!validStructureColumns(data)) throw new Error('The column list was invalid.');
      for (const column of data.columns) {
        const option = element('option', '', column.name);
        option.value = column.name;
        columnSelect.append(option);
      }
    } catch (error) {
      byId('fk-form-status').textContent = error.message || 'Could not list the columns of that table.';
    }
  }

  async function submitForeignKeyForm() {
    if (state.table === null || state.database === null) return;
    const database = state.database;
    const table = state.table;
    const status = byId('fk-form-status');
    const column = byId('fk-column-0').value;
    const referencedDatabase = byId('fk-ref-db-0').value;
    const referencedTable = byId('fk-ref-table-0').value;
    const referencedColumn = byId('fk-ref-column-0').value;
    if (!column || !referencedDatabase || !referencedTable || !referencedColumn) {
      status.textContent = 'Choose the local column, the referenced table and the referenced column.';
      return;
    }
    if (!window.confirm(`Add a foreign key on column ${JSON.stringify(column)} of table ${JSON.stringify(table)}, referencing table ${JSON.stringify(referencedTable)} in database ${JSON.stringify(referencedDatabase)}?`)) return;
    try {
      const data = await request('/api/foreign-key/add', {
        method: 'POST', requestID: newRequestID(),
        body: {
          database, table, name: byId('fk-name').value.trim(),
          columns: [column],
          referenced_database: referencedDatabase, referenced_table: referencedTable,
          referenced_columns: [referencedColumn],
          on_delete: byId('fk-on-delete').value, on_update: byId('fk-on-update').value,
          confirmed: true,
        },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      byId('fk-form').hidden = true;
      loadForeignKeys();
      loadStructure();
    } catch (error) {
      status.textContent = error.message || 'Could not add the foreign key.';
    }
  }

  // ---------------------------------------------------------- rename / drop table

  function showRenameTableForm() {
    if (state.table === null) return;
    byId('drop-table-form').hidden = true;
    byId('column-form').hidden = true;
    byId('index-form').hidden = true;
    byId('rename-table-form').hidden = false;
    byId('rename-table-input').value = state.table;
    byId('rename-table-status').textContent = '';
    byId('rename-table-input').focus();
    byId('rename-table-input').select();
  }

  async function submitRenameTable() {
    if (state.table === null || state.database === null) return;
    const newName = byId('rename-table-input').value.trim();
    const status = byId('rename-table-status');
    if (!newName || newName === state.table) { status.textContent = 'Enter a new table name.'; return; }
    const database = state.database;
    const oldName = state.table;
    if (!window.confirm(`Rename table ${JSON.stringify(oldName)} to ${JSON.stringify(newName)} in database ${JSON.stringify(database)}?`)) return;
    status.textContent = 'Renaming…';
    try {
      const data = await request('/api/table/rename', {
        method: 'POST', requestID: newRequestID(),
        body: { database, table: oldName, new_name: newName, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the rename.');
      byId('rename-table-form').hidden = true;
      state.table = newName;
      await refreshTablesList();
      updateWorkspaceContext();
      renderSidebar();
      loadStructure();
      loadRows(1);
    } catch (error) {
      status.textContent = error.message || 'Could not rename the table.';
    }
  }

  function showDropTableForm() {
    if (state.table === null) return;
    byId('rename-table-form').hidden = true;
    byId('column-form').hidden = true;
    byId('index-form').hidden = true;
    byId('drop-table-form').hidden = false;
    byId('drop-table-input').value = '';
    byId('drop-table-confirm').disabled = true;
    byId('drop-table-input').focus();
  }

  async function submitDropTable() {
    if (state.table === null || state.database === null) return;
    if (byId('drop-table-input').value !== state.table) return;
    const database = state.database;
    const table = state.table;
    if (!window.confirm(`Drop table ${JSON.stringify(table)} from database ${JSON.stringify(database)}? The table and all of its data are destroyed and this cannot be undone.`)) return;
    try {
      const data = await request('/api/table/drop', {
        method: 'POST', requestID: newRequestID(),
        body: { database, table, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      byId('drop-table-form').hidden = true;
      selectDatabase(database);
    } catch (error) {
      const status = byId('structure-status');
      status.textContent = `${error.message || 'Could not drop the table.'} The outcome may be unknown. Inspect the data before retrying.`;
      status.classList.add('is-error');
    }
  }

  // ---------------------------------------------------------- column editor

  function populateColumnTypeSelect() {
    const select = byId('column-form-type');
    const current = select.value;
    select.replaceChildren();
    const placeholder = element('option', '', '— type —');
    placeholder.value = '';
    select.append(placeholder);
    for (const type of COLUMN_TYPES) {
      const option = element('option', '', type);
      option.value = type;
      select.append(option);
    }
    if (COLUMN_TYPES.includes(current)) select.value = current;
  }

  function parseColumnType(type) {
    const match = /^([a-z]+)(?:\((\d+)(?:,(\d+))?\))?/.exec(String(type || '').toLowerCase().trim());
    if (!match) return { type: '', length: '', decimals: '' };
    return { type: match[1] || '', length: match[2] || '', decimals: match[3] || '' };
  }

  function openColumnForm(mode, column) {
    if (state.table === null || state.database === null) return;
    columnFormMode = mode;
    columnFormOriginal = mode === 'modify' && column ? column.name : null;
    byId('rename-table-form').hidden = true;
    byId('drop-table-form').hidden = true;
    byId('index-form').hidden = true;
    byId('column-form').hidden = false;
    byId('column-form-title').textContent = mode === 'modify' ? 'Modify column' : 'Add column';
    byId('column-form-submit').textContent = mode === 'modify' ? 'Change column' : 'Add column';
    byId('column-form-position-field').hidden = mode !== 'add';
    byId('column-form-position').value = '';
    byId('column-form-after-field').hidden = true;
    byId('column-form-status').textContent = '';
    populateColumnTypeSelect();
    const afterSelect = byId('column-form-after');
    afterSelect.replaceChildren();
    for (const entry of state.structureColumns || []) {
      const option = element('option', '', entry.name);
      option.value = entry.name;
      afterSelect.append(option);
    }
    if (mode === 'modify' && column) {
      const parsed = parseColumnType(column.type);
      byId('column-form-name').value = column.name;
      byId('column-form-type').value = COLUMN_TYPES.includes(parsed.type) ? parsed.type : '';
      byId('column-form-length').value = parsed.length;
      byId('column-form-decimals').value = parsed.decimals;
      byId('column-form-nullable').checked = column.nullable;
      byId('column-form-auto').checked = /auto_increment/i.test(column.extra);
      byId('column-form-primary').checked = column.key === 'PRI';
      byId('column-form-unique').checked = column.key === 'UNI';
      byId('column-form-default').value = column.default === null ? '' : column.default;
      byId('column-form-default-null').checked = column.default === null && column.nullable;
    } else {
      byId('column-form-name').value = '';
      byId('column-form-type').value = '';
      byId('column-form-length').value = '';
      byId('column-form-decimals').value = '';
      byId('column-form-nullable').checked = false;
      byId('column-form-auto').checked = false;
      byId('column-form-primary').checked = false;
      byId('column-form-unique').checked = false;
      byId('column-form-default').value = '';
      byId('column-form-default-null').checked = false;
    }
    byId('column-form-name').focus();
  }

  async function submitColumnForm() {
    if (state.table === null || state.database === null || !columnFormMode) return;
    const status = byId('column-form-status');
    const name = byId('column-form-name').value.trim();
    const type = byId('column-form-type').value;
    if (!name) { status.textContent = 'Enter a column name.'; byId('column-form-name').focus(); return; }
    if (!type) { status.textContent = 'Choose a column type.'; byId('column-form-type').focus(); return; }
    if (['varchar', 'char', 'binary', 'varbinary'].includes(type) && !/^\d{1,3}$/.test(byId('column-form-length').value.trim())) {
      status.textContent = 'This type needs a length (for example 30).';
      byId('column-form-length').focus();
      return;
    }
    const column = {
      name,
      type,
      length: byId('column-form-length').value.trim(),
      decimals: byId('column-form-decimals').value.trim(),
      nullable: byId('column-form-nullable').checked,
      auto_increment: byId('column-form-auto').checked,
      primary: byId('column-form-primary').checked,
      unique: byId('column-form-unique').checked,
      default: byId('column-form-default').value,
      default_null: byId('column-form-default-null').checked,
    };
    const database = state.database;
    const table = state.table;
    const mode = columnFormMode;
    const original = columnFormOriginal;
    let body;
    if (mode === 'modify') {
      body = { database, table, old_name: original, column, confirmed: true };
    } else {
      const position = byId('column-form-position').value;
      if (position === 'after' && !byId('column-form-after').value) {
        status.textContent = 'Choose the column to place this after.';
        return;
      }
      body = { database, table, column, position, confirmed: true };
      if (position === 'after') body.after_column = byId('column-form-after').value;
    }
    const actionText = mode === 'modify'
      ? `Change column ${JSON.stringify(name)} in table ${JSON.stringify(table)} in database ${JSON.stringify(database)}?`
      : `Add column ${JSON.stringify(name)} to table ${JSON.stringify(table)} in database ${JSON.stringify(database)}?`;
    if (!window.confirm(`${actionText} This modifies the table structure.`)) return;
    status.textContent = 'Saving…';
    try {
      const data = await request(mode === 'modify' ? '/api/column/modify' : '/api/column/add', {
        method: 'POST', requestID: newRequestID(), body,
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      byId('column-form').hidden = true;
      columnFormMode = null;
      columnFormOriginal = null;
      loadStructure();
      loadRows(mode === 'modify' ? (state.page || 1) : 1);
    } catch (error) {
      status.textContent = `${error.message || 'Could not change the column.'} The outcome may be unknown. Inspect the data before retrying.`;
    }
  }

  async function dropColumn(column) {
    if (state.table === null || state.database === null) return;
    const database = state.database;
    const table = state.table;
    if (!window.confirm(`Drop column ${JSON.stringify(column.name)} from table ${JSON.stringify(table)} in database ${JSON.stringify(database)}? Data stored in this column is destroyed and this cannot be undone.`)) return;
    const status = byId('structure-status');
    status.classList.remove('is-error');
    status.textContent = 'Dropping column…';
    try {
      const data = await request('/api/column/drop', {
        method: 'POST', requestID: newRequestID(),
        body: { database, table, column: column.name, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      loadStructure();
      loadRows(1);
    } catch (error) {
      status.textContent = `${error.message || 'Could not drop the column.'} The outcome may be unknown. Inspect the data before retrying.`;
      status.classList.add('is-error');
    }
  }

  // ---------------------------------------------------------------- row editor

  function buildRowEditorFields(form, specs) {
    form.replaceChildren();
    for (const spec of specs) {
      const position = rowEditor.fields.length;
      const field = element('div', 'row-editor-field');
      const labelRow = element('div', 'row-editor-field-label');
      const valueId = `row-editor-field-${position}-value`;
      const label = element('label', '', spec.column.name);
      label.setAttribute('for', valueId);
      labelRow.append(label);
      labelRow.append(element('span', 'row-editor-type', spec.column.type));
      if (spec.binary) labelRow.append(element('span', 'binary-hint', 'hex'));
      if (spec.column.key === 'PRI') labelRow.append(element('span', 'badge', 'primary'));
      field.append(labelRow);
      const input = element('input');
      input.id = valueId;
      input.spellcheck = false;
      input.value = spec.value === null ? '' : spec.value;
      if (spec.truncated) input.readOnly = true;
      field.append(input);
      let nullCheckbox = null;
      if (spec.column.nullable) {
        const nullLabel = element('label', 'check-label');
        nullCheckbox = document.createElement('input');
        nullCheckbox.type = 'checkbox';
        nullCheckbox.id = `row-editor-field-${position}-null`;
        // Insert mode starts blank: NULL is an explicit user action, and an
        // untouched field is omitted so the column default applies.
        nullCheckbox.checked = rowEditor && rowEditor.mode === 'insert' ? false : spec.value === null;
        nullCheckbox.disabled = spec.truncated;
        nullLabel.append(nullCheckbox, element('span', '', 'NULL'));
        field.append(nullLabel);
      }
      if (spec.truncated) {
        field.append(element('p', 'truncated-note',
          'This value was truncated for display. It is read-only here; saving leaves this column unchanged.'));
      }
      const entry = {
        column: spec.column,
        input,
        nullCheckbox,
        binary: spec.binary,
        truncated: spec.truncated,
        initialValue: spec.value,
        // Insert mode starts from "no value": any explicit NULL or typed text
        // counts as a change, so initialNull is false there.
        initialNull: rowEditor && rowEditor.mode === 'insert' ? false : spec.value === null,
      };
      const markDirty = () => {
        if (entry.truncated || !rowEditor) return;
        const nowNull = nullCheckbox ? nullCheckbox.checked : false;
        const initialText = entry.initialValue === null ? '' : entry.initialValue;
        if (nowNull !== entry.initialNull || (!nowNull && input.value !== initialText)) {
          rowEditor.dirty.add(entry.column.name);
        } else {
          rowEditor.dirty.delete(entry.column.name);
        }
        updateRowEditorSave();
      };
      input.addEventListener('input', markDirty);
      if (nullCheckbox) {
        nullCheckbox.addEventListener('change', () => {
          input.disabled = nullCheckbox.checked;
          if (nullCheckbox.checked) input.value = '';
          markDirty();
        });
        if (nullCheckbox.checked) input.disabled = true;
      }
      rowEditor.fields.push(entry);
      form.append(field);
    }
  }

  function updateRowEditorSave() {
    if (!rowEditor) return;
    byId('row-editor-save').disabled = rowEditor.mode === 'edit' && rowEditor.dirty.size === 0;
  }

  async function openRowEditor(mode, values) {
    if (state.table === null || state.database === null) return;
    const serial = ++rowEditorSerial;
    const database = state.database;
    const table = state.table;
    const keyPairs = mode === 'insert' ? [] : primaryKeyPairs(values);
    if (mode !== 'insert' && !keyPairs) return;
    const status = byId('row-editor-status');
    status.classList.remove('is-error');
    status.textContent = '';
    byId('row-editor-title').textContent = mode === 'insert' ? 'Insert row' : mode === 'copy' ? 'Copy row' : 'Edit row';
    byId('row-editor-save').textContent = mode === 'edit' ? 'Save changes' : 'Insert row';
    byId('row-editor-scope').textContent = `Table ${JSON.stringify(table)} in database ${JSON.stringify(database)}.`;
    openModal('row-editor-modal');
    if (mode === 'insert') {
      const columns = state.structureColumns || [];
      rowEditor = { mode, database, table, keyPairs: null, keyObject: null, fields: [], dirty: new Set() };
      buildRowEditorFields(byId('row-editor-form'), columns.map((column) => ({
        column, value: null, binary: binaryColumnType(column.type), truncated: false,
      })));
      status.textContent = 'Enter values for the new row. Empty fields use the column default.';
      updateRowEditorSave();
      return;
    }
    status.textContent = 'Loading the row…';
    try {
      const params = new URLSearchParams({ database, table, key: JSON.stringify(keyPairs) });
      const data = await request(`/api/row?${params}`);
      if (!validStructureColumns(data) || !Array.isArray(data.values) || data.values.length !== data.columns.length ||
        !Array.isArray(data.binary) || data.binary.length !== data.columns.length ||
        !Array.isArray(data.truncated) || data.truncated.length !== data.columns.length ||
        !data.values.every((value) => value === null || typeof value === 'string') ||
        !data.binary.every((flag) => typeof flag === 'boolean') ||
        !data.truncated.every((flag) => typeof flag === 'boolean')) {
        throw new Error('The row response was invalid.');
      }
      // A newer editor must not be clobbered by a slow row fetch.
      if (!state.connected || serial !== rowEditorSerial || byId('row-editor-modal').hidden) {
        if (byId('row-editor-modal').hidden) closeModal('row-editor-modal');
        return;
      }
      rowEditor = {
        mode, database, table, keyPairs,
        keyObject: Object.fromEntries(keyPairs),
        fields: [],
        dirty: new Set(),
      };
      buildRowEditorFields(byId('row-editor-form'), data.columns.map((column, index) => ({
        column, value: data.values[index], binary: data.binary[index], truncated: data.truncated[index],
      })));
      status.textContent = mode === 'copy'
        ? 'Adjust the values, then insert them as a new row.'
        : 'Change values and save. Unchanged columns are left as they are.';
      updateRowEditorSave();
    } catch (error) {
      closeRowEditor();
      showBrowseError(error);
    }
  }

  async function submitRowEditor() {
    if (!rowEditor || state.table === null || state.database === null) return;
    const editor = rowEditor;
    const { mode, database, table, keyObject, fields, dirty } = editor;
    const status = byId('row-editor-status');
    status.classList.remove('is-error');
    const values = {};
    const include = (entry) => {
      if (mode === 'copy') return true;
      if (mode === 'edit') return dirty.has(entry.column.name);
      return dirty.has(entry.column.name); // untouched insert fields use the column default
    };
    for (const entry of fields) {
      if (!include(entry)) continue;
      const isNull = entry.nullCheckbox ? entry.nullCheckbox.checked : false;
      if (isNull) {
        values[entry.column.name] = { null: true, value: '', binary: entry.binary };
        continue;
      }
      const value = entry.input.value;
      if (entry.binary && value !== '' && (!/^[0-9a-fA-F]*$/.test(value) || value.length % 2 !== 0)) {
        status.textContent = `Column ${JSON.stringify(entry.column.name)} needs an even-length hexadecimal value.`;
        status.classList.add('is-error');
        entry.input.focus();
        return;
      }
      values[entry.column.name] = { null: false, value, binary: entry.binary };
    }
    if (mode === 'edit' && !Object.keys(values).length) {
      status.textContent = 'No changes to save.';
      return;
    }
    const actionText = mode === 'edit'
      ? `Save changes to a row in table ${JSON.stringify(table)} in database ${JSON.stringify(database)}?`
      : `Insert a new row into table ${JSON.stringify(table)} in database ${JSON.stringify(database)}?`;
    if (!window.confirm(actionText)) return;
    status.textContent = 'Saving…';
    try {
      const body = mode === 'edit'
        ? { database, table, key: keyObject, values, confirmed: true }
        : { database, table, values, confirmed: true };
      const data = await request(mode === 'edit' ? '/api/row/update' : '/api/row/insert', {
        method: 'POST', requestID: newRequestID(), body,
      });
      // A slow save must not clobber an editor the user opened meanwhile.
      if (rowEditor !== editor) return;
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      closeModal('row-editor-modal');
      rowEditor = null;
      loadRows(state.page || 1);
    } catch (error) {
      if (rowEditor !== editor) return;
      status.textContent = `${error.message || 'Could not save the row.'} The outcome may be unknown. Inspect the data before retrying.`;
      status.classList.add('is-error');
    }
  }

  async function deleteRow(values) {
    if (state.table === null || state.database === null) return;
    const keyPairs = primaryKeyPairs(values);
    if (!keyPairs) return;
    const database = state.database;
    const table = state.table;
    if (!window.confirm(`Delete a row from table ${JSON.stringify(table)} in database ${JSON.stringify(database)}? This cannot be undone.`)) return;
    const status = byId('browse-status');
    status.classList.remove('is-error');
    status.textContent = 'Deleting…';
    try {
      const data = await request('/api/row/delete', {
        method: 'POST', requestID: newRequestID(),
        body: { database, table, key: Object.fromEntries(keyPairs), confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the deletion.');
      loadRows(state.page || 1);
    } catch (error) {
      status.textContent = `${error.message || 'Could not delete the row.'} The outcome may be unknown. Inspect the data before retrying.`;
      status.classList.add('is-error');
    }
  }

  // ---------------------------------------------------------------- objects tab

  const OBJECT_KINDS = [
    { key: 'routines', title: 'Routines (procedures and functions)' },
    { key: 'triggers', title: 'Triggers' },
    { key: 'events', title: 'Events' },
  ];

  async function loadObjects() {
    if (state.database === null) return;
    const serial = ++objectsRequest;
    const database = state.database;
    const status = byId('objects-status');
    byId('objects-database').textContent = database;
    status.classList.remove('is-error');
    status.textContent = 'Loading objects…';
    byId('objects-result').replaceChildren();
    const settled = await Promise.allSettled(OBJECT_KINDS.map(async (kind) => {
      const params = new URLSearchParams({ database });
      const data = await request(`/api/${kind.key}?${params}`);
      return { kind, data };
    }));
    if (!state.connected || serial !== objectsRequest || state.database !== database) return;
    let failures = 0;
    for (const [index, result] of settled.entries()) {
      const container = byId('objects-result');
      if (result.status !== 'fulfilled') {
        failures++;
        const block = element('div', 'section-block');
        block.append(element('h3', '', OBJECT_KINDS[index].title));
        block.append(element('p', 'hint-text', result.reason && result.reason.message
          ? result.reason.message : 'The object list was invalid.'));
        container.append(block);
        continue;
      }
      const { kind, data } = result.value;
      const list = kind.key === 'routines' ? data && data.routines : kind.key === 'triggers' ? data && data.triggers : data && data.events;
      if (!Array.isArray(list) || !list.every((item) => item && typeof item === 'object')) {
        failures++;
        const block = element('div', 'section-block');
        block.append(element('h3', '', kind.title));
        block.append(element('p', 'hint-text', 'The object list was invalid.'));
        container.append(block);
        continue;
      }
      const block = element('div', 'section-block');
      block.append(element('h3', '', kind.title));
      if (!list.length) {
        block.append(element('p', 'hint-text', 'None found.'));
      } else {
        for (const item of list) block.append(renderObjectItem(kind, item));
      }
      container.append(block);
    }
    filterObjects();
    status.textContent = failures
      ? `${failures} of the object lists could not be loaded.`
      : 'Routines, triggers and events for this database.';
  }

  function renderObjectItem(kind, item) {
    const row = element('div', 'object-item');
    let name = '';
    let kindLabel = '';
    let meta = '';
    let dropKind = '';
    if (kind.key === 'routines') {
      name = pick(item.name);
      kindLabel = pick(item.type);
      meta = [pick(item.created), pick(item.last_altered)].filter(Boolean).join(' · ');
      dropKind = kindLabel.toLowerCase();
    } else if (kind.key === 'triggers') {
      name = pick(item.name, item.trigger_name);
      kindLabel = 'trigger';
      meta = [pick(item.timing, item.action_timing), pick(item.event, item.event_manipulation, item.manipulation),
        pick(item.table, item.event_object_table)].filter(Boolean).join(' · ');
      dropKind = 'trigger';
    } else {
      name = pick(item.name, item.event_name);
      kindLabel = pick(item.type, item.event_type);
      const interval = [pick(item.interval_value), pick(item.interval_field)].filter(Boolean).join(' ');
      meta = [pick(item.status), interval].filter(Boolean).join(' · ');
      dropKind = 'event';
    }
    row.append(element('span', 'object-name', name));
    row.dataset.search = `${name} ${dropKind} ${meta}`.toLowerCase();
    if (kindLabel) row.append(element('span', 'badge', kindLabel));
    if (meta) row.append(element('span', 'object-meta', meta));
    if (kind.key === 'routines' && (dropKind === 'procedure' || dropKind === 'function')) {
      const definition = element('button', 'button button-secondary', 'Definition');
      definition.type = 'button';
      definition.addEventListener('click', () => showRoutineDefinition(name, dropKind));
      row.append(definition);
    }
    if (name && ['procedure', 'trigger', 'event'].includes(dropKind)) {
      const edit = element('button', 'button button-secondary', 'Edit');
      edit.type = 'button';
      edit.dataset.action = 'edit';
      edit.setAttribute('aria-label', `Edit ${dropKind} ${name}`);
      edit.addEventListener('click', () => openObjectEditor(dropKind, name));
      row.append(edit);
    }
    if (!name || !['procedure', 'function', 'trigger', 'event'].includes(dropKind)) return row;
    const drop = element('button', 'button button-secondary button-danger', 'Drop');
    drop.type = 'button';
    drop.addEventListener('click', () => dropRoutine(name, dropKind));
    drop.setAttribute('aria-label', `Drop ${dropKind} ${name}`);
    row.append(drop);
    return row;
  }

  function filterObjects() {
    const query = byId('objects-filter').value.trim().toLowerCase();
    for (const row of byId('objects-result').querySelectorAll('.object-item')) row.hidden = !row.dataset.search.includes(query);
  }

  async function getObjectDefinition(database, name, kind) {
    const params = new URLSearchParams({ database, name, kind });
    const data = await request(`/api/object/definition?${params}`);
    const definitionKey = { procedure: 'create procedure', trigger: 'sql original statement', event: 'create event' }[kind];
    const metadata = data && data.metadata;
    const allowed = [kind, definitionKey, 'sql_mode', 'character_set_client', 'collation_connection', 'database collation',
      ...(kind === 'event' ? ['time_zone'] : []), ...(kind === 'trigger' ? ['created'] : [])];
    if (!data || data.database !== database || data.name !== name || data.kind !== kind || data.truncated !== false ||
        !Number.isSafeInteger(data.generation) || data.generation < 0 || data.generation !== state.generation ||
        typeof data.definition !== 'string' || !data.definition.trim() || !data.definition.isWellFormed() ||
        !metadata || typeof metadata !== 'object' || Array.isArray(metadata) ||
        Object.entries(metadata).some(([key, value]) => !allowed.includes(key) || typeof value !== 'string' || !value.isWellFormed()) ||
        metadata[kind] !== name || metadata[definitionKey] !== data.definition ||
        !Object.hasOwn(metadata, 'sql_mode') || !metadata.character_set_client || !metadata.collation_connection ||
        !metadata['database collation'] || (kind === 'event' && !metadata.time_zone)) {
      throw new Error('The complete matching definition was not available; no change was made.');
    }
    const freezeSnapshot = (value) => {
      if (value && typeof value === 'object' && !Object.isFrozen(value)) {
        Object.values(value).forEach(freezeSnapshot);
        Object.freeze(value);
      }
      return value;
    };
    return freezeSnapshot(data);
  }

  const OBJECT_TEMPLATES = {
    procedure: 'CREATE PROCEDURE new_procedure()\nBEGIN\n  SELECT 1;\nEND',
    trigger: 'CREATE TRIGGER new_trigger BEFORE INSERT ON your_table\nFOR EACH ROW\nBEGIN\n  SET NEW.your_column = NEW.your_column;\nEND',
    event: 'CREATE EVENT new_event\nON SCHEDULE EVERY 1 DAY\nDO\nBEGIN\n  SELECT 1;\nEND',
  };

  function objectEditorCurrent(editor) {
    return objectEditor === editor && editor.serial === objectEditorSerial && state.connected && state.generation === editor.generation &&
      state.database === editor.database && byId('server-address').textContent === editor.server && !byId('object-editor-modal').hidden;
  }

  function downloadOriginal(editor) {
    const text = editor.original && editor.original.definition;
    if (typeof text !== 'string' || !text.isWellFormed()) return;
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
    const link = element('a');
    link.href = url;
    link.download = 'original-definition.sql';
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  function closeObjectEditor(preserve = true) {
    if (preserve && objectEditor && (objectEditor.original || objectEditor.outcome)) showBackgroundObjectOutcome({ ...objectEditor, name: objectEditor.outcomeName || objectEditor.name }, `${objectEditor.outcome || ''}${objectEditor.original ? ' Original definition retained.' : ''}`.trim());
    objectEditor = null;
    ++objectEditorSerial;
    closeModal('object-editor-modal');
  }

  function showBackgroundObjectOutcome(editor, message, succeeded = false) {
    const notice = element('div', succeeded ? 'notice workspace-notice' : 'notice notice-error workspace-notice');
    notice.setAttribute('role', succeeded ? 'status' : 'alert');
    notice.append(element('p', '', `${editor.kind} ${JSON.stringify(editor.name || '(new)')} in database ${JSON.stringify(editor.database)} on server ${JSON.stringify(editor.server)}: ${message}${succeeded ? '' : ' Inspect the server before retrying.'}`));
    if (editor.original !== null) {
      const label = element('label', '', 'Original definition (copyable)');
      const original = element('textarea', 'object-background-original');
      original.readOnly = true;
      original.rows = 5;
      original.value = editor.original.definition;
      label.append(original);
      notice.append(label);
      if (editor.original.definition.includes('\r')) notice.append(element('p', '', 'Textareas normalize carriage returns; use the lossless download.'));
      const download = element('button', 'button button-secondary', 'Download original SQL');
      download.type = 'button';
      download.addEventListener('click', () => downloadOriginal(editor));
      notice.append(download);
    }
    const dismiss = element('button', 'button button-secondary', 'Dismiss');
    dismiss.type = 'button';
    dismiss.addEventListener('click', () => notice.remove());
    notice.append(dismiss);
    byId('query-outcome-warnings').append(notice);
  }

  const OBJECT_REPLACEMENT_TOO_LARGE = 'Request exceeds the 1 MiB JSON byte limit: the original definition is duplicated in metadata. Reduce the proposed SQL if possible; the original remains available to copy or download.';

  function replacementExceedsLimit(editor, sql) {
    return new TextEncoder().encode(JSON.stringify({ original: editor.original, sql, confirmed: true })).length > 1024 * 1024;
  }

  function updateReplacementSize(editor) {
    if (!editor || !editor.name || !editor.original || !objectEditorCurrent(editor)) return;
    const tooLarge = replacementExceedsLimit(editor, byId('object-sql').value);
    const status = byId('object-editor-status');
    if (tooLarge && !editor.locked && !editor.original.definition.includes('\r')) status.textContent = OBJECT_REPLACEMENT_TOO_LARGE;
    else if (!tooLarge && status.textContent === OBJECT_REPLACEMENT_TOO_LARGE) status.textContent = 'Review and save the original before replacing.';
    byId('object-editor-submit').disabled = tooLarge || editor.locked || editor.original.definition.includes('\r') || objectOperation;
  }

  async function openObjectEditor(kind, name = '') {
    if (!state.connected || state.database === null || objectOperation) return;
    const editor = { kind, name, database: state.database, server: byId('server-address').textContent, generation: state.generation,
      serial: ++objectEditorSerial, original: null, locked: false, generatedTemplate: name ? null : OBJECT_TEMPLATES[kind] };
    editor.lockKey = JSON.stringify([editor.server, editor.database, editor.kind, editor.name]);
    editor.locked = !!name && replacementLocks.has(editor.lockKey);
    objectEditor = editor;
    byId('object-editor-title').textContent = `${name ? 'Edit' : 'Create'} ${kind}`;
    byId('object-editor-scope').textContent = `${kind} ${name ? JSON.stringify(name) : '(new)'} · database ${JSON.stringify(editor.database)} · server ${JSON.stringify(editor.server)}`;
    byId('object-original-field').hidden = !name;
    byId('object-replace-warning').hidden = !name;
    byId('object-name-field').hidden = !!name;
    byId('object-name').value = `new_${kind}`;
    byId('object-original').value = '';
    byId('object-sql').value = name ? '' : OBJECT_TEMPLATES[kind];
    byId('object-sql').readOnly = !!name;
    byId('object-editor-status').textContent = name ? 'Loading complete definition…' : '';
    byId('object-editor-submit').disabled = !!name;
    openModal('object-editor-modal');
    byId(name ? 'object-original' : 'object-name').focus();
    if (!name) return;
    try {
      const original = await getObjectDefinition(editor.database, name, kind);
      if (!objectEditorCurrent(editor)) return;
      editor.original = original;
      byId('object-original').value = original.definition;
      byId('object-sql').value = original.definition;
      const hasCR = original.definition.includes('\r');
      byId('object-sql').readOnly = hasCR || editor.locked;
      byId('object-editor-status').textContent = hasCR
        ? 'This definition contains carriage returns which textareas normalize. Replacement is disabled; use Download original SQL for a lossless copy.'
        : editor.locked ? 'Replacement is locked after an uncertain or failed operation. Inspect the server before retrying.' : 'Review and save the original before replacing.';
      byId('object-editor-submit').disabled = hasCR || editor.locked;
      updateReplacementSize(editor);
    } catch (error) {
      if (objectEditorCurrent(editor)) byId('object-editor-status').textContent = error.message || 'Definition unavailable.';
    }
  }

  async function submitObjectEditor() {
    const editor = objectEditor;
    if (!editor || !objectEditorCurrent(editor) || objectOperation || editor.locked) return;
    const replacing = !!editor.name;
    if (!Number.isSafeInteger(editor.generation) || editor.generation < 0) {
      byId('object-editor-status').textContent = 'Connection generation is unavailable; reconnect before creating or replacing.';
      return;
    }
    if (replacing && (!editor.original || editor.original.definition.includes('\r') || replacementLocks.has(editor.lockKey))) return;
    const sql = byId('object-sql').value;
    const name = replacing ? editor.name : byId('object-name').value;
    const status = byId('object-editor-status');
    if (!sql.trim() || !name) { status.textContent = 'Enter an intended name and SQL.'; return; }
    const operationLockKey = JSON.stringify([editor.server, editor.database, editor.kind, name]);
    if (replacementLocks.has(operationLockKey)) {
      editor.locked = true;
      byId('object-editor-submit').disabled = true;
      status.textContent = 'This object is locked after an uncertain operation. Inspect the server before retrying.';
      return;
    }
    const body = replacing ? { original: editor.original, sql, confirmed: true } :
      { database: editor.database, name, kind: editor.kind, generation: editor.generation, sql, confirmed: true };
    if (new TextEncoder().encode(JSON.stringify(body)).length > 1024 * 1024) {
      status.textContent = replacing ? OBJECT_REPLACEMENT_TOO_LARGE : 'Request exceeds the 1 MiB JSON byte limit; reduce the SQL before confirming.';
      if (replacing) byId('object-editor-submit').disabled = true;
      return;
    }
    const scope = `${editor.kind} ${JSON.stringify(name)} in database ${JSON.stringify(editor.database)} on server ${JSON.stringify(editor.server)}`;
    const warning = replacing
      ? 'DROP + CREATE is non-atomic. External DDL can race; grants can be lost, trigger order and event schedules can change. No automatic restore or retry. SQL is unsandboxed.'
      : 'Execute this unsandboxed SQL with the database account privileges. The CREATE target is checked, but its body and privileges are not sandboxed.';
    if (!window.confirm(`${replacing ? 'Replace' : 'Create'} ${scope}?\n\n${warning}`)) return;
    if (!objectEditorCurrent(editor)) return;
    objectOperation = true;
    byId('object-editor-submit').disabled = true;
    try {
      status.textContent = replacing ? 'Replacing object…' : 'Executing proposed SQL…';
      const result = await request(replacing ? '/api/object/replace' : '/api/object/create', { method: 'POST', requestID: newRequestID(), body });
      if (replacing ? (!result || result.stage !== 'replaced' || Object.hasOwn(result, 'error')) :
        (!result || result.stage !== 'created' || Object.hasOwn(result, 'error'))) {
        const error = new Error('The server did not confirm the resulting object.');
        error.data = result;
        throw error;
      }
      if (!objectEditorCurrent(editor)) { showBackgroundObjectOutcome({ ...editor, name }, replacing ? 'Replacement completed successfully.' : 'CREATE completed successfully; the object exists, but its definition was not compared to the submitted SQL.', true); return; }
      showBackgroundObjectOutcome({ ...editor, name }, replacing ? 'Replacement completed successfully.' : 'CREATE completed successfully; the object exists, but its definition was not compared to the submitted SQL.', true);
      closeObjectEditor(false);
      loadObjects();
    } catch (error) {
      const stage = error.data && error.data.stage;
      const safeToRetry = replacing && stage === 'not_started';
      let message;
      if (replacing) {
        message = stage === 'not_started' ? 'Replacement did not start. Correct the problem before retrying.' :
          stage === 'drop_unknown' ? 'DROP outcome is unknown; the object may be missing.' :
          stage === 'dropped_create_failed' ? 'DROP completed but CREATE failed; the object is missing.' :
          stage === 'create_unknown' ? 'DROP completed but CREATE outcome is unknown; the object may be missing or changed.' :
          stage === 'replay_unknown' ? 'The previous attempt outcome is unknown; DROP or CREATE may have happened.' :
          stage === 'created_unverified' ? 'CREATE completed, but the resulting object could not be verified; it may be missing or changed.' :
          'Replacement outcome is unknown; DROP or CREATE may have happened.';
      } else message = stage === 'created_unverified'
        ? 'CREATE completed, but the resulting object could not be verified; it may be missing or changed.'
        : 'CREATE failed or its outcome is uncertain.';
      if (!safeToRetry) replacementLocks.add(operationLockKey);
      editor.locked = !safeToRetry;
      const text = `${error.message || 'Operation failed.'} ${message}${safeToRetry ? '' : ' Inspect the server before retrying.'}`;
      editor.outcome = text;
      editor.outcomeName = name;
      if (!objectEditorCurrent(editor)) { showBackgroundObjectOutcome({ ...editor, name }, text); return; }
      status.textContent = text;
    } finally {
      objectOperation = false;
      if (objectEditorCurrent(editor)) byId('object-editor-submit').disabled = editor.locked;
      if (objectEditorCurrent(editor)) updateReplacementSize(editor);
    }
  }

  async function showRoutineDefinition(name, kind) {
    if (state.database === null) return;
    const database = state.database;
    const serial = ++definitionSerial;
    const server = byId('server-address').textContent;
    const current = () => serial === definitionSerial && !byId('definition-modal').hidden && state.connected &&
      state.database === database && byId('server-address').textContent === server;
    byId('definition-title').textContent = 'Definition';
    byId('definition-scope').textContent = `${kind} ${JSON.stringify(name)} in database ${JSON.stringify(database)}.`;
    byId('definition-note').textContent = '';
    byId('definition-code').textContent = 'Loading…';
    openModal('definition-modal');
    try {
      const params = new URLSearchParams({ database, name, kind });
      const data = await request(`/api/routine?${params}`);
      if (!current()) return;
      if (!data || data.name !== name || typeof data.definition !== 'string') {
        throw new Error('The definition response was invalid.');
      }
      byId('definition-code').textContent = data.definition;
      if (data.truncated === true) {
        byId('definition-note').textContent = 'The definition was clipped at the display limit.';
      }
    } catch (error) {
      if (!current()) return;
      byId('definition-code').textContent = '';
      byId('definition-note').textContent = error.message || 'Could not load the definition.';
    }
  }

  async function dropRoutine(name, kind) {
    if (state.database === null || objectOperation) return;
    const database = state.database;
    const server = byId('server-address').textContent;
    const serial = objectEditorSerial;
    const status = byId('objects-status');
    objectOperation = true;
    status.classList.remove('is-error');
    status.textContent = `Checking ${kind}…`;
    try {
      if (kind !== 'function') await getObjectDefinition(database, name, kind);
      if (!state.connected || state.database !== database || serial !== objectEditorSerial || byId('server-address').textContent !== server) return;
      if (!window.confirm(`Drop ${kind} ${JSON.stringify(name)} from database ${JSON.stringify(database)} on server ${JSON.stringify(server)}? This cannot be undone.`)) { status.textContent = 'Drop cancelled.'; return; }
      status.textContent = `Dropping ${kind}…`;
      const data = await request('/api/routine/drop', {
        method: 'POST', requestID: newRequestID(),
        body: { database, name, kind, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      if (state.connected && state.database === database && serial === objectEditorSerial) loadObjects();
    } catch (error) {
      if (state.connected && state.database === database && serial === objectEditorSerial) {
        status.textContent = `${error.message || `Could not drop the ${kind}.`} If DROP was sent, its outcome may be unknown. Inspect before retrying.`;
        status.classList.add('is-error');
      }
    } finally {
      objectOperation = false;
    }
  }

  // ---------------------------------------------------------------- search tab

  async function runSearch() {
    if (state.database === null) return;
    const q = byId('search-input').value.trim();
    const mode = byId('search-mode').value;
    const status = byId('search-status');
    const result = byId('search-result');
    status.classList.remove('is-error');
    if (!q) { status.textContent = 'Enter text to search for.'; byId('search-input').focus(); return; }
    if (q.length > 256) { status.textContent = 'Search text is limited to 256 characters.'; return; }
    const database = state.database;
    status.textContent = 'Searching…';
    result.replaceChildren();
    try {
      const params = new URLSearchParams({ database, q, mode });
      const data = await request(`/api/search?${params}`);
      if (!state.connected || state.database !== database) return;
      if (!data || !Array.isArray(data.results) || !data.results.every((entry) => entry &&
        typeof entry.table === 'string' && Array.isArray(entry.columns) &&
        entry.columns.every((column) => typeof column === 'string') && Array.isArray(entry.rows) &&
        entry.rows.every((row) => Array.isArray(row) && row.length === entry.columns.length &&
          row.every((value) => value === null || typeof value === 'string')) && typeof entry.more === 'boolean')) {
        throw new Error('The search response was invalid.');
      }
      byId('search-database').textContent = database;
      renderSearchResults(data.results, q);
      status.textContent = data.results.length
        ? `${data.results.length} ${data.results.length === 1 ? 'table has' : 'tables have'} matches.`
        : 'No matches in this database.';
    } catch (error) {
      status.textContent = error.message || 'Could not run the search.';
      status.classList.add('is-error');
    }
  }

  function renderSearchResults(results, q) {
    const container = byId('search-result');
    container.replaceChildren();
    for (const entry of results) {
      const block = element('div', 'search-table-result');
      const heading = element('div', 'search-result-heading');
      heading.append(element('h3', '', `${entry.table} (${entry.rows.length} ${entry.rows.length === 1 ? 'row' : 'rows'})`));
      const openButton = element('button', 'button button-secondary', 'Open in Browse');
      openButton.type = 'button';
      openButton.addEventListener('click', () => openSearchResultInBrowse(entry.table, q));
      heading.append(openButton);
      block.append(heading);
      const grid = element('div', 'grid-scroll');
      renderGrid(grid, entry.columns, entry.rows);
      block.append(grid);
      if (entry.more) {
        block.append(element('p', 'search-result-note', 'More matching rows exist in this table than are shown.'));
      }
      container.append(block);
    }
  }

  async function openSearchResultInBrowse(tableName, q) {
    if (state.database === null) return;
    let filterColumn = '';
    try {
      const params = new URLSearchParams({ database: state.database, table: tableName });
      const data = await request(`/api/structure?${params}`);
      if (validStructureColumns(data)) {
        const column = data.columns.find((entry) => stringColumnType(entry.type));
        if (column) filterColumn = column.name;
      }
    } catch {
      // Open the table without a prefilled filter.
    }
    if (filterColumn) {
      const select = byId('filter-column-0');
      if (!Array.from(select.options).some((option) => option.value === filterColumn)) {
        const option = element('option', '', filterColumn);
        option.value = filterColumn;
        select.append(option);
      }
      select.value = filterColumn;
      byId('filter-op-0').value = 'like';
      const valueInput = byId('filter-value-0');
      valueInput.value = q;
      valueInput.disabled = false;
      selectTable(tableName, { keepFilters: true });
    } else {
      selectTable(tableName);
    }
  }

  // ---------------------------------------------------------------- status tab

  const validPair = (row) => Array.isArray(row) && row.length === 2 && row.every((value) => value === null || typeof value === 'string');

  function renderProcessGrid(rows) {
    const container = byId('processes-result');
    container.replaceChildren();
    if (!rows.length) {
      container.append(element('p', 'hint-text', 'No processes reported.'));
      return;
    }
    const table = element('table', 'data-grid');
    const head = element('thead');
    const headRow = element('tr');
    for (const label of ['Id', 'User', 'Host', 'Database', 'Command', 'Time', 'State', 'Info', 'Actions']) {
      const cell = element('th', '', label);
      cell.scope = 'col';
      headRow.append(cell);
    }
    head.append(headRow);
    const body = element('tbody');
    for (const row of rows) {
      const tr = element('tr');
      for (const value of row) {
        const cell = element('td');
        appendValue(cell, value);
        tr.append(cell);
      }
      const actions = element('td');
      const wrap = element('div', 'row-actions');
      const kill = element('button', 'button button-secondary button-danger', 'Kill');
      kill.type = 'button';
      kill.addEventListener('click', () => killProcess(row));
      wrap.append(kill);
      actions.append(wrap);
      tr.append(actions);
      body.append(tr);
    }
    table.append(head, body);
    container.append(table);
  }

  async function loadStatus() {
    if (!state.connected) return;
    const serial = ++statusRequest;
    const status = byId('status-status');
    status.classList.remove('is-error');
    status.textContent = 'Loading server status…';
    try {
      const data = await request('/api/status');
      if (!state.connected || serial !== statusRequest) return;
      if (!data || !Array.isArray(data.status) || !Array.isArray(data.variables) || !Array.isArray(data.processes) ||
        !data.status.every(validPair) || !data.variables.every(validPair) ||
        !data.processes.every((row) => Array.isArray(row) && row.length === 8 &&
          row.every((value) => value === null || typeof value === 'string'))) {
        throw new Error('The status response was invalid.');
      }
      renderProcessGrid(data.processes);
      renderGrid(byId('variables-result'), ['Name', 'Value'], data.variables);
      renderGrid(byId('server-status-result'), ['Name', 'Value'], data.status);
      status.textContent = `${data.processes.length} ${data.processes.length === 1 ? 'process' : 'processes'} · ${data.variables.length} variables · ${data.status.length} status counters.`;
    } catch (error) {
      status.textContent = error.message || 'Could not load the server status.';
      status.classList.add('is-error');
    }
  }

  async function killProcess(row) {
    const id = row[0];
    const status = byId('status-status');
    status.classList.remove('is-error');
    if (!/^\d+$/.test(String(id))) {
      status.textContent = 'This process has no numeric id and cannot be killed.';
      return;
    }
    const database = row[3];
    const target = `MySQL process ${id}${database ? ` on database ${JSON.stringify(database)}` : ''}`;
    if (!window.confirm(`Kill ${target}? The statement it is running is terminated and this cannot be undone.`)) return;
    status.textContent = `Killing process ${id}…`;
    try {
      const data = await request('/api/process/kill', {
        method: 'POST', requestID: newRequestID(),
        body: { id: Number(id), confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the kill.');
      loadStatus();
    } catch (error) {
      status.textContent = `${error.message || 'Could not kill the process.'} The outcome may be unknown. Inspect the data before retrying.`;
      status.classList.add('is-error');
    }
  }

  // ---------------------------------------------------------------- users

  const PRIVILEGE_LIST = ['ALL', 'SELECT', 'INSERT', 'UPDATE', 'DELETE', 'CREATE', 'DROP', 'ALTER', 'INDEX',
    'REFERENCES', 'CREATE VIEW', 'SHOW VIEW', 'CREATE ROUTINE', 'ALTER ROUTINE', 'EXECUTE', 'TRIGGER', 'EVENT',
    'LOCK TABLES', 'CREATE TEMPORARY TABLES', 'SHOW DATABASES'];

  let privilegeTarget = null;  // { user, host }
  let privilegeMode = 'grant'; // 'grant' | 'revoke'

  async function loadUsers() {
    if (!state.connected) return;
    const serial = ++usersRequest;
    const status = byId('users-status');
    status.classList.remove('is-error');
    status.textContent = 'Loading database accounts…';
    try {
      const data = await request('/api/users');
      if (!state.connected || serial !== usersRequest) return;
      if (!data || !Array.isArray(data.users) || typeof data.restricted !== 'boolean' ||
        !data.users.every((account) => account && typeof account.user === 'string' && typeof account.host === 'string')) {
        throw new Error('The account list was invalid.');
      }
      const restricted = byId('users-restricted');
      const myGrants = byId('my-grants');
      restricted.hidden = !data.restricted;
      myGrants.hidden = !data.restricted;
      renderUsers(data.users);
      status.textContent = data.restricted
        ? 'This account cannot list database users.'
        : `${data.users.length} ${data.users.length === 1 ? 'account' : 'accounts'}.`;
      if (data.restricted) loadMyGrants();
    } catch (error) {
      if (!state.connected || serial !== usersRequest) return;
      status.textContent = error.message || 'Could not load the accounts.';
      status.classList.add('is-error');
    }
  }

  async function loadMyGrants() {
    const container = byId('my-grants-result');
    container.replaceChildren();
    try {
      const data = await request('/api/grants');
      if (!data || !Array.isArray(data.grants) || !data.grants.every((line) => typeof line === 'string')) {
        throw new Error('The grants response was invalid.');
      }
      for (const line of data.grants) container.append(element('code', 'grant-line', line));
      if (!data.grants.length) container.append(element('p', 'hint-text', 'No grants reported.'));
    } catch (error) {
      container.append(element('p', 'hint-text', error.message || 'Could not load the grants.'));
    }
  }

  function renderUsers(users) {
    const container = byId('users-result');
    container.replaceChildren();
    if (!users.length) {
      container.append(element('p', 'hint-text', 'No accounts visible to this connection.'));
      return;
    }
    const list = element('div', 'fk-list');
    for (const account of users) {
      const item = element('div', 'fk-item');
      item.append(element('span', 'fk-name', `${account.user}@${account.host}`));
      const actions = element('div', 'row-actions');
      for (const [label, handler] of [
        ['Grants', () => showUserGrants(account.user, account.host)],
        ['Password', () => openPasswordModal(account.user, account.host)],
        ['Grant', () => openPrivilegeModal(account.user, account.host, 'grant')],
        ['Revoke', () => openPrivilegeModal(account.user, account.host, 'revoke')],
        ['Drop', () => openUserDropModal(account.user, account.host)],
      ]) {
        const button = element('button', label === 'Drop' ? 'button button-danger' : 'button button-secondary', label);
        button.type = 'button';
        button.addEventListener('click', handler);
        actions.append(button);
      }
      item.append(actions);
      list.append(item);
    }
    container.append(list);
  }

  async function showUserGrants(user, host) {
    const scope = byId('user-grants-scope');
    const container = byId('user-grants-result');
    const status = byId('user-grants-status');
    status.textContent = '';
    container.replaceChildren();
    scope.textContent = `Grants for ${JSON.stringify(user)}@${JSON.stringify(host)}.`;
    openModal('user-grants-modal');
    try {
      const params = new URLSearchParams({ user, host });
      const data = await request(`/api/grants?${params}`);
      if (!data || !Array.isArray(data.grants) || !data.grants.every((line) => typeof line === 'string')) {
        throw new Error('The grants response was invalid.');
      }
      for (const line of data.grants) container.append(element('code', 'grant-line', line));
      if (!data.grants.length) status.textContent = 'No grants reported.';
    } catch (error) {
      status.textContent = error.message || 'Could not load the grants.';
    }
  }

  function openPasswordModal(user, host) {
    byId('user-password-scope').textContent = `Set a new password for ${JSON.stringify(user)}@${JSON.stringify(host)}.`;
    byId('user-password-value').value = '';
    byId('user-password-status').textContent = '';
    openModal('user-password-modal');
  }

  async function submitPasswordChange() {
    if (!state.connected) return;
    const user = parseAccountScope(byId('user-password-scope'));
    const password = byId('user-password-value').value;
    const status = byId('user-password-status');
    if (!user) return;
    if (!password) {
      status.textContent = 'Enter the new password.';
      return;
    }
    if (!window.confirm(`Set a new password for account ${JSON.stringify(user.user)}@${JSON.stringify(user.host)} on the database server?`)) return;
    status.textContent = 'Saving…';
    try {
      const data = await request('/api/user/password', {
        method: 'POST', requestID: newRequestID(),
        body: { user: user.user, host: user.host, password, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      byId('user-password-value').value = '';
      closeModal('user-password-modal');
    } catch (error) {
      status.textContent = `${error.message || 'Could not set the password.'} The outcome may be unknown. Inspect the account before retrying.`;
    }
  }

  function openUserCreate() {
    if (!state.connected) return;
    byId('user-create-name').value = '';
    byId('user-create-host').value = '127.0.0.1';
    byId('user-create-password').value = '';
    byId('user-create-status').textContent = '';
    openModal('user-create-modal');
  }

  async function submitUserCreate() {
    if (!state.connected) return;
    const user = byId('user-create-name').value.trim();
    const host = byId('user-create-host').value.trim();
    const password = byId('user-create-password').value;
    const status = byId('user-create-status');
    if (!user || !host) {
      status.textContent = 'A user name and a host are required.';
      return;
    }
    if (!window.confirm(`Create account ${JSON.stringify(user)}@${JSON.stringify(host)} on the database server?`)) return;
    status.textContent = 'Creating…';
    try {
      const data = await request('/api/user/create', {
        method: 'POST', requestID: newRequestID(),
        body: { user, host, password, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      byId('user-create-password').value = '';
      closeModal('user-create-modal');
      loadUsers();
    } catch (error) {
      status.textContent = `${error.message || 'Could not create the account.'} The outcome may be unknown. Inspect the accounts before retrying.`;
    }
  }

  function openPrivilegeModal(user, host, mode) {
    if (!state.connected) return;
    privilegeTarget = { user, host };
    privilegeMode = mode;
    byId('privilege-title').textContent = mode === 'grant' ? 'Grant privileges' : 'Revoke privileges';
    byId('privilege-scope').textContent = `${mode === 'grant' ? 'Grant to' : 'Revoke from'} ${JSON.stringify(user)}@${JSON.stringify(host)}.`;
    byId('privilege-scope-select').value = 'global';
    byId('privilege-database-field').hidden = true;
    byId('privilege-table-field').hidden = true;
    byId('privilege-database').value = state.database || '';
    byId('privilege-table').value = '';
    byId('privilege-grant-option').checked = false;
    byId('privilege-status').textContent = '';
    const grid = byId('privilege-checkboxes');
    grid.replaceChildren();
    for (const privilege of PRIVILEGE_LIST) {
      const label = element('label', '', privilege);
      const checkbox = element('input');
      checkbox.type = 'checkbox';
      checkbox.value = privilege;
      checkbox.checked = privilege === 'SELECT' && mode === 'grant';
      label.prepend(checkbox);
      grid.append(label);
    }
    openModal('privilege-modal');
  }

  async function submitPrivilegeChange() {
    if (!state.connected || privilegeTarget === null) return;
    const { user, host } = privilegeTarget;
    const status = byId('privilege-status');
    const privileges = Array.from(byId('privilege-checkboxes').querySelectorAll('input:checked'))
      .map((checkbox) => checkbox.value);
    if (!privileges.length) {
      status.textContent = 'Choose at least one privilege.';
      return;
    }
    const scopeMode = byId('privilege-scope-select').value;
    const database = scopeMode === 'global' ? '' : byId('privilege-database').value.trim();
    const table = scopeMode === 'table' ? byId('privilege-table').value.trim() : '';
    if (scopeMode !== 'global' && !database) {
      status.textContent = 'A database name is required for this scope.';
      return;
    }
    if (scopeMode === 'table' && !table) {
      status.textContent = 'A table name is required for this scope.';
      return;
    }
    const grantOption = byId('privilege-grant-option').checked;
    const verb = privilegeMode === 'grant' ? 'Grant' : 'Revoke';
    const scopeText = scopeMode === 'global' ? '*.*'
      : scopeMode === 'database' ? `${JSON.stringify(database)}.*`
        : `${JSON.stringify(database)}.${JSON.stringify(table)}`;
    if (!window.confirm(`${verb} ${privileges.join(', ')} on ${scopeText} ${privilegeMode === 'grant' ? 'to' : 'from'} account ${JSON.stringify(user)}@${JSON.stringify(host)}${grantOption ? ' (WITH GRANT OPTION)' : ''}?`)) return;
    status.textContent = 'Applying…';
    try {
      const data = await request(privilegeMode === 'grant' ? '/api/grant' : '/api/revoke', {
        method: 'POST', requestID: newRequestID(),
        body: { user, host, privileges, database, table, grant_option: grantOption, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      privilegeTarget = null;
      closeModal('privilege-modal');
      loadUsers();
    } catch (error) {
      status.textContent = `${error.message || 'Could not apply the privilege change.'} The outcome may be unknown. Inspect the account before retrying.`;
    }
  }

  function openUserDropModal(user, host) {
    if (!state.connected) return;
    byId('user-drop-scope').textContent = `Drop account ${JSON.stringify(user)}@${JSON.stringify(host)} from the database server.`;
    byId('user-drop-confirm').value = '';
    byId('user-drop-submit').disabled = true;
    byId('user-drop-status').textContent = '';
    byId('user-drop-confirm').dataset.expected = `${user}@${host}`;
    openModal('user-drop-modal');
  }

  async function submitUserDrop() {
    if (!state.connected) return;
    const account = parseAccountScope(byId('user-drop-scope'));
    const status = byId('user-drop-status');
    if (!account) return;
    const { user, host } = account;
    if (!window.confirm(`Drop account ${JSON.stringify(user)}@${JSON.stringify(host)} from the database server? The account can no longer connect. This cannot be undone.`)) return;
    status.textContent = 'Dropping…';
    try {
      const data = await request('/api/user/drop', {
        method: 'POST', requestID: newRequestID(),
        body: { user, host, confirmed: true },
      });
      if (!data || typeof data.affected_rows !== 'number') throw new Error('The server did not confirm the change.');
      closeModal('user-drop-modal');
      loadUsers();
    } catch (error) {
      status.textContent = `${error.message || 'Could not drop the account.'} The outcome may be unknown. Inspect the accounts before retrying.`;
    }
  }

  // parseAccountScope reads user/host back from a modal scope line formatted
  // as `... "user"@"host".` — the modal text is the single source of truth.
  function parseAccountScope(node) {
    const match = /"((?:[^"\\]|\\.)*)"@"((?:[^"\\]|\\.)*)"/.exec(node.textContent);
    if (!match) return null;
    return { user: JSON.parse(`"${match[1]}"`), host: JSON.parse(`"${match[2]}"`) };
  }

  // ---------------------------------------------------------------- SQL tab: history + explain

  function renderHistory() {
    const list = byId('history-list');
    const status = byId('history-status');
    list.replaceChildren();
    if (!state.history.length) {
      status.textContent = 'No statements yet.';
      return;
    }
    status.textContent = `${state.history.length} ${state.history.length === 1 ? 'statement' : 'statements'}, newest first.`;
    for (const entry of [...state.history].reverse()) {
      const item = element('li');
      const button = element('button', 'history-item');
      button.type = 'button';
      button.append(element('span', 'history-sql', entry.sql));
      const metaBits = [];
      if (typeof entry.duration_ms === 'number') metaBits.push(`${entry.duration_ms} ms`);
      if (typeof entry.rows === 'number') metaBits.push(`${entry.rows} rows`);
      else if (typeof entry.affected === 'number') metaBits.push(`${entry.affected} affected`);
      if (metaBits.length) button.append(element('span', 'history-meta', metaBits.join(' · ')));
      const sql = entry.sql;
      button.addEventListener('click', () => {
        byId('sql-editor').value = sql;
        highlightSQL();
        byId('sql-editor').focus();
      });
      item.append(button);
      list.append(item);
    }
  }

  async function refreshHistory() {
    const status = byId('history-status');
    if (!state.connected) {
      state.history = [];
      renderHistory();
      return;
    }
    try {
      const data = await request('/api/history');
      if (!state.connected) return;
      if (!data || !Array.isArray(data.history) || !data.history.every((entry) => entry && typeof entry.sql === 'string')) {
        state.history = [];
        byId('history-list').replaceChildren();
        status.textContent = 'History is unavailable.';
        return;
      }
      state.history = data.history;
      renderHistory();
    } catch {
      if (!state.connected) return;
      state.history = [];
      byId('history-list').replaceChildren();
      status.textContent = 'History is unavailable.';
    }
  }

  async function clearHistory() {
    if (!window.confirm('Clear the SQL history for this session?')) return;
    try {
      const data = await request('/api/history/clear', {
        method: 'POST', requestID: newRequestID(), body: {},
      });
      if (!data || data.cleared !== true) throw new Error('The server did not confirm the clear.');
      state.history = [];
      renderHistory();
    } catch {
      byId('history-status').textContent = 'Could not clear the history.';
    }
  }

  function explainQuery() {
    const editor = byId('sql-editor');
    const sql = editor.value.trim();
    const status = byId('query-status');
    if (!sql) {
      status.textContent = 'Enter a SQL statement first.';
      status.classList.add('is-error');
      editor.focus();
      return;
    }
    editor.value = `EXPLAIN ${sql}`;
    highlightSQL();
    byId('sql-form').requestSubmit();
  }

  // ---------------------------------------------------------------- SQL highlighting

  const SQL_KEYWORDS = new Set(('accessible add all alter analyze and as asc asensitive before between bigint '
    + 'binary blob both by call cascade case change char character check collate column condition connection '
    + 'constraint continue convert create cross current_date current_time current_timestamp current_user cursor '
    + 'database databases day_hour day_microsecond day_minute day_second dec decimal declare default delayed '
    + 'delete desc describe deterministic distinct distinctrow div double drop dual each else elseif enclosed '
    + 'escaped exists exit explain fetch float float4 float8 for force foreign from fulltext grant group '
    + 'having high_priority hour_microsecond hour_minute hour_second if ignore in index infile inner inout '
    + 'insensitive insert int int1 int2 int3 int4 int8 integer interval into is iterate join key keys kill '
    + 'leading leave left like limit linear lines load localtime localtimestamp lock long longblob longtext '
    + 'loop low_priority master_ssl_verify_server_cert match mediumblob mediumint mediumtext middleint minute_'
    + 'microsecond minute_second mod modifies natural not no_write_to_binlog null numeric on optimize option '
    + 'optionally or order out outer outfile partition precision primary procedure purge range read reads '
    + 'read_only read_write real references regexp release rename repeat replace require restrict return '
    + 'revoke right rlike schema schemas second_microsecond select sensitive separator set show smallint '
    + 'spatial specific sql sql_big_result sql_calc_found_rows sql_small_result sqlexception sqlstate '
    + 'sqlwarning ssl starting straight_join table terminated then tinyblob tinyint tinytext to trailing '
    + 'trigger undo union unique unlock unsigned update usage use using utc_date utc_time utc_timestamp values '
    + 'varbinary varchar varcharacter varying view where while with write year').split(' '));

  // tokenizeSQL splits SQL text into typed fragments. Data is rendered into
  // spans with createElement/textContent only — never markup injection.
  function tokenizeSQL(text) {
    const tokens = [];
    let plain = '';
    let index = 0;
    const flushPlain = () => {
      if (plain) {
        tokens.push({ type: '', value: plain });
        plain = '';
      }
    };
    while (index < text.length) {
      const character = text[index];
      const rest = text.slice(index);
      if (character === "'" || character === '"' || character === '`') {
        let position = index + 1;
        let closed = false;
        while (position < text.length) {
          if (text[position] === '\\') {
            position += 2;
            continue;
          }
          if (text[position] === character) {
            if (text[position + 1] === character) {
              position += 2;
              continue;
            }
            position += 1;
            closed = true;
            break;
          }
          position += 1;
        }
        const value = closed ? text.slice(index, position) : text.slice(index);
        flushPlain();
        tokens.push({ type: character === '`' ? 'tok-ident' : 'tok-string', value });
        index += value.length;
        continue;
      }
      if (rest.startsWith('/*')) {
        const end = text.indexOf('*/', index + 2);
        const stop = end === -1 ? text.length : end + 2;
        flushPlain();
        tokens.push({ type: 'tok-comment', value: text.slice(index, stop) });
        index = stop;
        continue;
      }
      const before = index === 0 ? '' : text[index - 1];
      if ((rest.startsWith('--') && (before === '' || /\s/.test(before)) || rest.startsWith('#'))) {
        const end = text.indexOf('\n', index);
        const stop = end === -1 ? text.length : end;
        flushPlain();
        tokens.push({ type: 'tok-comment', value: text.slice(index, stop) });
        index = stop;
        continue;
      }
      if (/[0-9]/.test(character) && !/[\w$]/.test(before)) {
        const match = /^(?:0[xX][0-9a-fA-F]+|[0-9]+(?:\.[0-9]*)?(?:[eE][+-]?[0-9]+)?|\.[0-9]+(?:[eE][+-]?[0-9]+)?)/.exec(rest);
        if (match) {
          flushPlain();
          tokens.push({ type: 'tok-number', value: match[0] });
          index += match[0].length;
          continue;
        }
      }
      if (/[A-Za-z_$]/.test(character)) {
        const match = /^[A-Za-z_$][A-Za-z0-9_$]*/.exec(rest);
        const word = match[0];
        if (SQL_KEYWORDS.has(word.toLowerCase())) {
          flushPlain();
          tokens.push({ type: 'tok-keyword', value: word });
          index += word.length;
          continue;
        }
      }
      plain += character;
      index += 1;
    }
    flushPlain();
    return tokens;
  }

  function highlightSQL() {
    const editor = byId('sql-editor');
    const overlay = byId('sql-highlight');
    if (!editor || !overlay) return;
    overlay.replaceChildren();
    const text = editor.value;
    for (const token of tokenizeSQL(text)) {
      if (token.type) overlay.append(element('span', token.type, token.value));
      else overlay.append(token.value);
    }
    if (text.endsWith('\n')) overlay.append(' ');
    overlay.scrollTop = editor.scrollTop;
    overlay.scrollLeft = editor.scrollLeft;
  }

  // ---------------------------------------------------------------- SQL execution

  function showBackgroundQueryError(database, server) {
    const notice = element('div', 'notice notice-error workspace-notice');
    notice.setAttribute('role', 'alert');
    notice.style.overflowWrap = 'anywhere';
    const message = element('span', '', `The SQL request for database ${JSON.stringify(database)} on server ${JSON.stringify(server)} failed or its response was lost. Its outcome may be unknown. Inspect the data before retrying. `);
    const dismiss = element('button', 'button button-secondary', 'Dismiss');
    dismiss.type = 'button';
    dismiss.addEventListener('click', () => notice.remove());
    notice.append(message, dismiss);
    byId('query-outcome-warnings').append(notice);
  }

  async function runQuery() {
    if (!state.connected || state.database === null || state.queryRunning) return;
    const sql = byId('sql-editor').value;
    const status = byId('query-status');
    if (!sql.trim()) {
      status.textContent = 'Enter a SQL statement first.';
      status.classList.add('is-error');
      byId('sql-editor').focus();
      return;
    }
    const database = state.database;
    const server = byId('server-address').textContent;
    if (!window.confirm(`Run SQL on database ${JSON.stringify(database)} at server ${JSON.stringify(server)}?\n\nSQL can modify or delete data and affect other databases or files, subject to MySQL permissions. Continue?`)) return;
    const serial = ++queryRequest;
    const button = byId('run-query');
    state.queryRunning = true;
    button.disabled = true;
    button.textContent = 'Running…';
    status.classList.remove('is-error');
    status.textContent = 'Running query…';
    byId('query-result').replaceChildren();
    byId('query-result').hidden = true;
    try {
      // A retry of the same HTTP request retains this ID; the server will not
      // execute it twice, even if the browser silently replays the POST.
      const requestID = newRequestID();
      const data = await request('/api/query', {
        method: 'POST',
        requestID,
        body: { database, sql, confirmed: true },
        networkFailureMessage: 'Could not reach DBKid. Check the connection.',
      });
      if (!validGrid(data) || !(data.affected_rows === null || (typeof data.affected_rows === 'number' && Number.isFinite(data.affected_rows))) ||
        (data.truncated !== undefined && typeof data.truncated !== 'boolean') || typeof data.duration_ms !== 'number' || !Number.isFinite(data.duration_ms)) {
        throw new Error('The query response was invalid.');
      }
      if (!state.connected || serial !== queryRequest) return;
      const summary = [];
      if (data.columns.length) {
        renderGrid(byId('query-result'), data.columns, data.rows);
        byId('query-result').hidden = false;
        summary.push(`${data.rows.length} ${data.rows.length === 1 ? 'row' : 'rows'} returned`);
      }
      if (data.affected_rows !== null) summary.push(`${data.affected_rows} ${data.affected_rows === 1 ? 'row' : 'rows'} affected`);
      if (!summary.length) summary.push('Query completed');
      summary.push(`${data.duration_ms} ms`);
      if (data.truncated) summary.push('Results truncated (result limit reached)');
      if (data.cells_truncated === true) summary.push('Some cell values truncated');
      status.textContent = `${summary.join(' · ')}.`;
    } catch (error) {
      if (!state.connected || serial !== queryRequest || byId('sql-panel').hidden) showBackgroundQueryError(database, server);
      if (!state.connected || serial !== queryRequest) return;
      status.textContent = `${error.message || 'Could not run SQL.'} The outcome may be unknown. Inspect the data before retrying.`;
      status.classList.add('is-error');
    } finally {
      if (state.connected && serial === queryRequest) {
        state.queryRunning = false;
        button.disabled = false;
        button.textContent = 'Run SQL';
        refreshHistory();
      }
    }
  }

  // ---------------------------------------------------------------- export

  function openExportDialog(table) {
    if (state.database === null) return;
    exportScope = { table: table || null };
    const format = byId('export-format');
    const what = byId('export-what');
    const status = byId('export-status');
    status.classList.remove('is-error');
    status.textContent = '';
    const csvOption = format.querySelector('option[value="csv"]');
    if (csvOption) csvOption.disabled = exportScope.table === null;
    format.value = exportScope.table === null ? 'sql' : 'csv';
    if (format.value === 'csv') {
      what.value = 'data';
      what.disabled = true;
    } else {
      what.disabled = false;
    }
    byId('export-scope').textContent = exportScope.table
      ? `Exporting table ${JSON.stringify(exportScope.table)} from database ${JSON.stringify(state.database)}.`
      : `Exporting the whole database ${JSON.stringify(state.database)}.`;
    openModal('export-modal');
  }

  async function submitExport() {
    if (!exportScope || state.database === null) return;
    const database = state.database;
    const table = exportScope.table || '';
    const format = byId('export-format').value;
    const what = format === 'csv' ? 'data' : byId('export-what').value;
    const status = byId('export-status');
    status.classList.remove('is-error');
    let sort = '';
    let dir = '';
    let filter = '';
    if (table && format === 'csv') {
      if (state.sortColumn) {
        sort = state.sortColumn;
        dir = state.sortDir === 'desc' ? 'desc' : 'asc';
      }
      const filters = activeFilters();
      if (filters.length) filter = JSON.stringify(filters);
    }
    status.textContent = 'Preparing the export…';
    try {
      const data = await request('/api/export/token', {
        method: 'POST',
        requestID: newRequestID(),
        body: { database, table, format, what, sort, dir, filter },
      });
      if (!data || typeof data.url !== 'string' || !data.url) throw new Error('The export response was invalid.');
      closeModal('export-modal');
      location.href = data.url;
    } catch (error) {
      status.textContent = error.message || 'Could not prepare the export.';
      status.classList.add('is-error');
    }
  }

  // ---------------------------------------------------------------- import

  function parseCSV(content, delimiter, quote, keepRows = Infinity) {
    const rows = [];
    let fieldCount = 0;
    let row = [];
    let field = '';
    let inQuotes = false;
    let started = false;
    const endField = () => {
      row.push(field);
      field = '';
      started = false;
    };
    const endRow = () => {
      endField();
      if (row.length > fieldCount) fieldCount = row.length;
      if (rows.length < keepRows) rows.push(row);
      row = [];
    };
    for (let index = 0; index < content.length; index++) {
      const ch = content[index];
      if (inQuotes) {
        if (ch === quote) {
          if (content[index + 1] === quote) {
            field += quote;
            index++;
          } else {
            inQuotes = false;
          }
          continue;
        }
        field += ch;
        continue;
      }
      if (!started && field === '' && ch === quote) {
        inQuotes = true;
        started = true;
        continue;
      }
      if (ch === delimiter) {
        endField();
        continue;
      }
      if (ch === '\r' || ch === '\n') {
        if (ch === '\r' && content[index + 1] === '\n') index++;
        endRow();
        continue;
      }
      started = true;
      field += ch;
    }
    if (started || row.length) endRow();
    return { rows, fieldCount, rowCount: rows.length };
  }

  function renderImportPreview() {
    const preview = byId('import-preview');
    preview.replaceChildren();
    if (!importState || !importState.content) return;
    preview.append(element('div', 'import-preview-file', importState.name));
    const kib = importState.size / 1024;
    const sizeText = importState.size >= 1024 * 1024
      ? `${importState.size.toLocaleString('en-US')} bytes (${(kib / 1024).toFixed(1)} MiB)`
      : `${importState.size.toLocaleString('en-US')} bytes (${kib.toFixed(1)} KiB)`;
    preview.append(element('div', 'hint-text', sizeText));
    if (importState.size > IMPORT_LIMIT_BYTES) {
      preview.append(element('div', 'hint-text', 'This file is larger than the 8 MiB import limit.'));
      return;
    }
    const firstLine = importState.content.split(/\r\n|\r|\n/, 1)[0] || '';
    const line = element('div', 'import-preview-line');
    line.textContent = `First line: ${firstLine.slice(0, 200)}${firstLine.length > 200 ? '…' : ''}`;
    preview.append(line);
  }

  function renderImportMapping() {
    const container = byId('import-mapping');
    container.replaceChildren();
    if (!importState || !importState.content) return;
    const delimiter = byId('import-delimiter').value;
    const quote = byId('import-quote').value;
    const analysis = parseCSV(importState.content, delimiter, quote, 5);
    importState.fieldCount = analysis.fieldCount;
    if (!analysis.fieldCount) return;
    const columns = state.structureColumns || [];
    for (let index = 0; index < analysis.fieldCount; index++) {
      const row = element('div', 'import-mapping-row');
      row.append(element('span', 'inline-label', `CSV field ${index + 1}`));
      const select = element('select');
      const skip = element('option', '', '— skip —');
      skip.value = '';
      select.append(skip);
      for (const column of columns) {
        const option = element('option', '', column.name);
        option.value = column.name;
        select.append(option);
      }
      select.value = columns[index] ? columns[index].name : '';
      row.append(select);
      container.append(row);
    }
  }

  function renderImportOptions() {
    if (!importState) return;
    const formatSelect = byId('import-format');
    const csvOption = formatSelect.querySelector('option[value="csv"]');
    if (csvOption) csvOption.disabled = state.table === null;
    if (formatSelect.value === 'csv' && state.table === null) formatSelect.value = 'sql';
    const format = formatSelect.value;
    byId('import-sql-options').hidden = format !== 'sql';
    byId('import-csv-options').hidden = format !== 'csv';
    renderImportPreview();
    if (format === 'csv') renderImportMapping();
  }

  function openImportDialog() {
    if (state.database === null) return;
    importState = { name: '', size: 0, content: '', format: state.table !== null ? 'csv' : 'sql', fieldCount: 0 };
    byId('import-scope').textContent = state.table
      ? `Import into table ${JSON.stringify(state.table)} in database ${JSON.stringify(state.database)}.`
      : `Import into database ${JSON.stringify(state.database)}. SQL only — no table is selected.`;
    byId('import-format').value = importState.format;
    byId('import-file').value = '';
    const status = byId('import-status');
    status.classList.remove('is-error');
    status.textContent = 'Choose a file to see a preview before anything is sent.';
    byId('import-preview').replaceChildren();
    byId('import-mapping').replaceChildren();
    renderImportOptions();
    openModal('import-modal');
  }

  async function handleImportFile() {
    if (!importState) return;
    const input = byId('import-file');
    const file = input.files && input.files[0];
    const status = byId('import-status');
    if (!file) {
      importState.name = '';
      importState.size = 0;
      importState.content = '';
      renderImportPreview();
      return;
    }
    status.classList.remove('is-error');
    status.textContent = 'Reading the file…';
    try {
      const content = await file.text();
      importState.name = file.name;
      importState.size = file.size;
      importState.content = content;
      if (/\.sql$/i.test(file.name)) {
        byId('import-format').value = 'sql';
      } else if (/\.(csv|txt)$/i.test(file.name) && state.table !== null) {
        byId('import-format').value = 'csv';
      }
      renderImportOptions();
      status.textContent = '';
    } catch {
      status.textContent = 'Could not read the file as text.';
      status.classList.add('is-error');
    }
  }

  async function submitImport() {
    if (!importState || state.database === null) return;
    const status = byId('import-status');
    status.classList.remove('is-error');
    if (!importState.content) {
      status.textContent = 'Choose a file first.';
      byId('import-file').focus();
      return;
    }
    if (importState.size > IMPORT_LIMIT_BYTES) {
      status.textContent = 'The file is larger than the 8 MiB import limit.';
      status.classList.add('is-error');
      return;
    }
    const database = state.database;
    const table = state.table || '';
    const format = byId('import-format').value;
    let body;
    if (format === 'sql') {
      body = {
        database,
        content: importState.content,
        stop_on_error: byId('import-stop-on-error').checked,
        confirmed: true,
      };
    } else {
      if (!table) {
        status.textContent = 'CSV import needs a selected table.';
        status.classList.add('is-error');
        return;
      }
      const columns = Array.from(byId('import-mapping').querySelectorAll('select'))
        .map((select) => select.value || null);
      if (!columns.length) {
        status.textContent = 'Choose a CSV file first.';
        status.classList.add('is-error');
        return;
      }
      body = {
        database,
        table,
        content: importState.content,
        has_header: byId('import-header').checked,
        delimiter: byId('import-delimiter').value,
        quote: byId('import-quote').value,
        columns,
        confirmed: true,
      };
    }
    const target = format === 'sql'
      ? `database ${JSON.stringify(database)}`
      : `table ${JSON.stringify(table)} in database ${JSON.stringify(database)}`;
    if (!window.confirm(`Import ${JSON.stringify(importState.name)} into ${target}? Statements in the file run immediately; this can modify or delete data and cannot be undone.`)) return;
    status.textContent = 'Importing…';
    try {
      const data = await request(format === 'sql' ? '/api/import/sql' : '/api/import/csv', {
        method: 'POST', requestID: newRequestID(), body,
      });
      if (format === 'sql') {
        if (!data || !Number.isInteger(data.executed) || !Number.isInteger(data.failed) || !Array.isArray(data.errors)) {
          throw new Error('The import response was invalid.');
        }
        const first = data.errors[0];
        status.textContent = `${data.executed} ${data.executed === 1 ? 'statement' : 'statements'} executed · ${data.failed} failed${data.stopped === true ? ' · stopped early' : ''}.${first ? ` First error — statement ${first.index}: ${first.error}` : ''}`;
      } else {
        if (!data || !Number.isInteger(data.inserted) || !Number.isInteger(data.failed) || !Array.isArray(data.errors)) {
          throw new Error('The import response was invalid.');
        }
        const first = data.errors[0];
        status.textContent = `${data.inserted} ${data.inserted === 1 ? 'row' : 'rows'} inserted · ${data.failed} failed${data.stopped === true ? ' · stopped early' : ''}.${first ? ` First error — row ${first.row}: ${first.error}` : ''}`;
      }
      if (table) {
        loadStructure();
        loadRows(1);
      }
    } catch (error) {
      status.textContent = `${error.message || 'Could not import the file.'} The outcome may be unknown. Inspect the data before retrying.`;
      status.classList.add('is-error');
    }
  }

  // ---------------------------------------------------------------- recent connections

  async function loadRecents() {
    const section = byId('recent-connections');
    const list = byId('recent-list');
    try {
      const data = await request('/api/recent');
      if (state.connected) return;
      if (!data || !Array.isArray(data.recent) || !data.recent.every((recent) => recent &&
        typeof recent.host === 'string' && typeof recent.port === 'number' &&
        typeof recent.user === 'string' && typeof recent.tls === 'string')) {
        section.hidden = true;
        return;
      }
      list.replaceChildren();
      if (!data.recent.length) {
        section.hidden = true;
        return;
      }
      for (const recent of data.recent) {
        const chip = element('button', 'recent-chip');
        chip.type = 'button';
        chip.append(element('span', '', `${recent.user}@${recent.host}:${recent.port}`));
        chip.append(element('span', 'badge', recent.tls === 'verify' ? 'tls' : 'plaintext'));
        chip.title = `Prefill ${recent.user}@${recent.host}:${recent.port}`;
        chip.addEventListener('click', () => {
          byId('host').value = recent.host;
          byId('port').value = String(recent.port);
          byId('user').value = recent.user;
          byId('tls').value = recent.tls === 'verify' ? 'verify' : 'disable';
          tlsChosenByUser = true;
          byId('password').value = '';
          byId('password').focus();
        });
        list.append(chip);
      }
      section.hidden = false;
    } catch {
      section.hidden = true;
    }
  }

  // ---------------------------------------------------------------- workspace lifecycle

  function showWorkspace(session) {
    state.connected = true;
    state.generation = Number.isSafeInteger(session.generation) && session.generation >= 0 ? session.generation : null;
    state.database = null;
    state.table = null;
    state.databases = [];
    state.tables = [];
    ++tableRequest;
    ++rowsRequest;
    ++structureRequest;
    ++indexesRequest;
    ++objectsRequest;
    ++statusRequest;
    state.view = 'browse';
    newDatabaseOpen = false;
    dropDatabaseOpen = false;
    sidebarToolsError = '';
    clearDatabaseContext();
    state.history = [];
    renderHistory();
    resetQuery();
    byId('server-address').textContent = `${session.user}@${session.host}:${session.port}`;
    byId('server-version').textContent = session.server_version ? `Server ${session.server_version}` : 'Server connected';
    bootView.hidden = true;
    connectView.hidden = true;
    appView.hidden = false;
    byId('recent-connections').hidden = true;
    updateWorkspaceContext();
    refreshHistory();
    loadDatabases();
  }

  async function disconnect() {
    if (!state.connected) return;
    const button = byId('disconnect-button');
    const errorBox = byId('disconnect-error');
    errorBox.hidden = true;
    button.disabled = true;
    button.textContent = 'Disconnecting…';
    try {
      const data = await request('/api/disconnect', { method: 'POST', body: {} });
      if (!data || data.connected !== false) throw new Error('The server did not confirm disconnection.');
      state.connected = false;
      state.generation = null;
      ++databaseRequest;
      ++tableRequest;
      ++rowsRequest;
      ++structureRequest;
      ++indexesRequest;
      ++objectsRequest;
      ++statusRequest;
      state.database = null;
      state.table = null;
      state.databases = [];
      state.tables = [];
      state.rowsLoading = false;
      state.hasNext = false;
      state.history = [];
      newDatabaseOpen = false;
      dropDatabaseOpen = false;
      sidebarToolsError = '';
      resetQuery();
      clearDatabaseContext();
      renderHistory();
      byId('database-list').replaceChildren();
      byId('server-address').textContent = '';
      byId('server-version').textContent = '';
      byId('password').value = '';
      byId('connect-error').hidden = true;
      tlsChosenByUser = false;
      byId('tls').value = byId('host').value === '127.0.0.1' ? 'disable' : 'verify';
      renderDatabaseTools();
      appView.hidden = true;
      connectView.hidden = true;
      csrfToken = '';
      await bootstrap();
      if (!connectView.hidden) byId('host').focus();
    } catch (error) {
      errorBox.textContent = error.message || 'Could not disconnect.';
      errorBox.hidden = false;
    } finally {
      button.disabled = false;
      button.textContent = 'Disconnect';
    }
  }

  async function bootstrap() {
    bootView.hidden = false;
    connectView.hidden = true;
    appView.hidden = true;
    retryBootstrap.hidden = true;
    bootMessage.textContent = 'Starting DBKid…';
    try {
      let response;
      try {
        response = await fetch('/api/session', { credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' } });
      } catch {
        throw new Error('Could not reach DBKid. Check the connection and try again.');
      }
      let data;
      try {
        data = await response.json();
      } catch {
        throw new Error('The server returned an invalid response.');
      }
      if (!response.ok) throw new Error(data && typeof data.error === 'string' ? data.error : 'Could not start the session.');
      if (!data || typeof data.csrf_token !== 'string' || !data.csrf_token || typeof data.connected !== 'boolean') {
        throw new Error('The session response was incomplete.');
      }
      csrfToken = data.csrf_token;
      bootView.hidden = true;
      if (data.connected) showWorkspace(data);
      else {
        connectView.hidden = false;
        loadRecents();
      }
    } catch (error) {
      bootMessage.textContent = `Could not start DBKid: ${error.message || 'Network error.'}`;
      retryBootstrap.hidden = false;
    }
  }

  // ---------------------------------------------------------------- wiring

  byId('host').addEventListener('input', () => {
    if (!tlsChosenByUser) byId('tls').value = byId('host').value === '127.0.0.1' ? 'disable' : 'verify';
  });
  byId('tls').addEventListener('change', () => { tlsChosenByUser = true; });

  byId('connect-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const button = byId('connect-button');
    const errorBox = byId('connect-error');
    const password = byId('password');
    const host = byId('host').value;
    if (!tlsChosenByUser) byId('tls').value = host === '127.0.0.1' ? 'disable' : 'verify';
    const tls = byId('tls').value;
    let allowInsecure = false;
    errorBox.hidden = true;
    if (!host || /\s/.test(host)) {
      errorBox.textContent = 'Enter a host without spaces.';
      errorBox.hidden = false;
      password.value = '';
      byId('host').focus();
      return;
    }
    if (tls === 'disable' && !isLoopbackHost(host)) {
      if (!window.confirm('TLS is disabled for a non-loopback host. Your username, password, and database traffic will be sent unencrypted. Continue?')) {
        password.value = '';
        return;
      }
      allowInsecure = true;
    }
    button.disabled = true;
    button.textContent = 'Connecting…';
    try {
      const data = await request('/api/connect', {
        method: 'POST',
        body: {
          host,
          port: Number(byId('port').value),
          user: byId('user').value,
          password: password.value,
          tls,
          allow_insecure: allowInsecure,
        },
      });
      if (!data || data.connected !== true) throw new Error('The server did not confirm the connection.');
      showWorkspace(data);
    } catch (error) {
      errorBox.textContent = error.message || 'Could not connect.';
      errorBox.hidden = false;
    } finally {
      password.value = '';
      button.disabled = false;
      button.textContent = 'Connect';
    }
  });

  byId('tab-browse').addEventListener('click', () => {
    if (state.table === null) return;
    state.view = 'browse';
    updateWorkspaceContext();
  });
  byId('tab-structure').addEventListener('click', () => {
    if (state.table === null) return;
    state.view = 'structure';
    updateWorkspaceContext();
    loadStructure();
  });
  byId('tab-sql').addEventListener('click', () => {
    if (state.database === null) return;
    state.view = 'sql';
    updateWorkspaceContext();
    refreshHistory();
  });
  byId('tab-objects').addEventListener('click', () => {
    if (state.database === null) return;
    state.view = 'objects';
    updateWorkspaceContext();
    loadObjects();
  });
  byId('tab-search').addEventListener('click', () => {
    if (state.database === null) return;
    state.view = 'search';
    updateWorkspaceContext();
    byId('search-input').focus();
  });
  byId('tab-status').addEventListener('click', () => {
    if (!state.connected) return;
    state.view = 'status';
    updateWorkspaceContext();
    loadStatus();
  });
  byId('tab-users').addEventListener('click', () => {
    if (!state.connected) return;
    state.view = 'users';
    updateWorkspaceContext();
    loadUsers();
  });
  byId('users-refresh').addEventListener('click', loadUsers);
  byId('table-create-close').addEventListener('click', () => closeModal('table-create-modal'));
  byId('table-create-cancel').addEventListener('click', () => closeModal('table-create-modal'));
  byId('table-create-submit').addEventListener('click', submitTableCreate);
  byId('table-create-add-column').addEventListener('click', addTableColumnRow);
  byId('user-create-button').addEventListener('click', openUserCreate);
  byId('user-create-submit').addEventListener('click', submitUserCreate);
  byId('user-create-cancel').addEventListener('click', () => closeModal('user-create-modal'));
  byId('user-create-close').addEventListener('click', () => closeModal('user-create-modal'));
  byId('user-password-submit').addEventListener('click', submitPasswordChange);
  byId('user-password-cancel').addEventListener('click', () => closeModal('user-password-modal'));
  byId('user-password-close').addEventListener('click', () => closeModal('user-password-modal'));
  byId('user-grants-cancel').addEventListener('click', () => closeModal('user-grants-modal'));
  byId('user-grants-close').addEventListener('click', () => closeModal('user-grants-modal'));
  byId('privilege-submit').addEventListener('click', submitPrivilegeChange);
  byId('privilege-cancel').addEventListener('click', () => closeModal('privilege-modal'));
  byId('privilege-close').addEventListener('click', () => closeModal('privilege-modal'));
  byId('privilege-scope-select').addEventListener('change', () => {
    const mode = byId('privilege-scope-select').value;
    byId('privilege-database-field').hidden = mode === 'global';
    byId('privilege-table-field').hidden = mode !== 'table';
  });
  byId('user-drop-submit').addEventListener('click', submitUserDrop);
  byId('user-drop-cancel').addEventListener('click', () => closeModal('user-drop-modal'));
  byId('user-drop-close').addEventListener('click', () => closeModal('user-drop-modal'));
  byId('user-drop-confirm').addEventListener('input', () => {
    byId('user-drop-submit').disabled = byId('user-drop-confirm').value !== byId('user-drop-confirm').dataset.expected;
  });
  byId('fk-add-button').addEventListener('click', openForeignKeyForm);
  byId('fk-form-cancel').addEventListener('click', () => { byId('fk-form').hidden = true; });
  byId('fk-form-submit').addEventListener('click', submitForeignKeyForm);
  byId('fk-ref-db-0').addEventListener('change', loadForeignKeyRefTables);
  byId('fk-ref-table-0').addEventListener('change', loadForeignKeyRefColumns);
  byId('sql-editor').addEventListener('input', highlightSQL);
  byId('sql-editor').addEventListener('scroll', () => {
    const overlay = byId('sql-highlight');
    const editor = byId('sql-editor');
    overlay.scrollTop = editor.scrollTop;
    overlay.scrollLeft = editor.scrollLeft;
  });
  highlightSQL();

  byId('sql-form').addEventListener('submit', (event) => {
    event.preventDefault();
    runQuery();
  });
  byId('explain-query').addEventListener('click', explainQuery);
  byId('clear-history').addEventListener('click', clearHistory);
  byId('sql-editor').addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      if (!state.queryRunning) byId('sql-form').requestSubmit();
    }
  });

  byId('retry-rows').addEventListener('click', () => {
    if (!state.rowsLoading) loadRows(state.requestedPage);
  });
  byId('prev-page').addEventListener('click', () => {
    if (!state.rowsLoading && state.page > 1) loadRows(state.page - 1);
  });
  byId('next-page').addEventListener('click', () => {
    if (!state.rowsLoading && state.hasNext) loadRows(state.page + 1);
  });
  byId('page-size').addEventListener('change', (event) => {
    const size = Number(event.target.value);
    if (![25, 50, 100, 200].includes(size)) return;
    state.pageSize = size;
    loadRows(1);
  });

  byId('filter-apply').addEventListener('click', () => {
    if (!state.rowsLoading && state.table !== null) loadRows(1);
  });
  byId('filter-clear').addEventListener('click', () => {
    resetFilterInputs();
    if (!state.rowsLoading && state.table !== null) loadRows(1);
  });
  for (let index = 0; index < 3; index++) {
    byId(`filter-op-${index}`).addEventListener('change', () => {
      const op = byId(`filter-op-${index}`).value;
      const input = byId(`filter-value-${index}`);
      input.disabled = op === 'is null' || op === 'is not null';
      if (input.disabled) input.value = '';
    });
  }

  byId('insert-row-button').addEventListener('click', () => openRowEditor('insert'));
  byId('browse-export-button').addEventListener('click', () => openExportDialog(state.table));
  byId('structure-export-button').addEventListener('click', () => openExportDialog(state.table));
  byId('browse-import-button').addEventListener('click', openImportDialog);
  byId('structure-import-button').addEventListener('click', openImportDialog);

  byId('add-column-button').addEventListener('click', () => openColumnForm('add', null));
  byId('rename-table-button').addEventListener('click', showRenameTableForm);
  byId('rename-table-confirm').addEventListener('click', submitRenameTable);
  byId('rename-table-cancel').addEventListener('click', () => { byId('rename-table-form').hidden = true; });
  byId('drop-table-button').addEventListener('click', showDropTableForm);
  byId('drop-table-input').addEventListener('input', () => {
    byId('drop-table-confirm').disabled = state.table === null || byId('drop-table-input').value !== state.table;
  });
  byId('drop-table-confirm').addEventListener('click', submitDropTable);
  byId('drop-table-cancel').addEventListener('click', () => { byId('drop-table-form').hidden = true; });
  byId('column-form-submit').addEventListener('click', submitColumnForm);
  byId('column-form-cancel').addEventListener('click', () => {
    byId('column-form').hidden = true;
    columnFormMode = null;
    columnFormOriginal = null;
  });
  byId('column-form-position').addEventListener('change', () => {
    byId('column-form-after-field').hidden = byId('column-form-position').value !== 'after';
  });

  byId('add-index-button').addEventListener('click', openIndexForm);
  byId('index-form-submit').addEventListener('click', submitIndexForm);
  byId('index-form-cancel').addEventListener('click', () => { byId('index-form').hidden = true; });

  byId('row-editor-save').addEventListener('click', submitRowEditor);
  byId('row-editor-cancel').addEventListener('click', closeRowEditor);
  byId('row-editor-close').addEventListener('click', closeRowEditor);

  byId('export-submit').addEventListener('click', submitExport);
  byId('export-cancel').addEventListener('click', () => closeModal('export-modal'));
  byId('export-close').addEventListener('click', () => closeModal('export-modal'));

  byId('import-submit').addEventListener('click', submitImport);
  byId('import-cancel').addEventListener('click', () => {
    closeModal('import-modal');
    importState = null;
  });
  byId('import-close').addEventListener('click', () => {
    closeModal('import-modal');
    importState = null;
  });
  byId('import-file').addEventListener('change', handleImportFile);
  byId('import-format').addEventListener('change', renderImportOptions);
  byId('import-delimiter').addEventListener('change', renderImportMapping);
  byId('import-quote').addEventListener('change', renderImportMapping);

  byId('definition-close').addEventListener('click', () => closeModal('definition-modal'));
  byId('definition-cancel').addEventListener('click', () => closeModal('definition-modal'));
  byId('objects-filter').addEventListener('input', filterObjects);
  byId('object-original-download').addEventListener('click', () => { if (objectEditor && objectEditor.original) downloadOriginal(objectEditor); });
  for (const kind of ['procedure', 'trigger', 'event']) byId(`object-create-${kind}`).addEventListener('click', () => openObjectEditor(kind));
  byId('object-name').addEventListener('input', () => {
    const editor = objectEditor;
    const field = byId('object-sql');
    if (!editor || editor.name || field.value !== editor.generatedTemplate) return;
    const quoted = '`' + byId('object-name').value.replace(/`/g, '``') + '`';
    editor.generatedTemplate = OBJECT_TEMPLATES[editor.kind].replace(`new_${editor.kind}`, quoted);
    field.value = editor.generatedTemplate;
  });
  byId('object-sql').addEventListener('input', () => updateReplacementSize(objectEditor));
  byId('object-editor-submit').addEventListener('click', submitObjectEditor);
  byId('object-editor-close').addEventListener('click', closeObjectEditor);
  byId('object-editor-cancel').addEventListener('click', closeObjectEditor);

  byId('search-form').addEventListener('submit', (event) => {
    event.preventDefault();
    runSearch();
  });
  byId('refresh-status').addEventListener('click', loadStatus);

  byId('disconnect-button').addEventListener('click', disconnect);
  byId('refresh-databases').addEventListener('click', loadDatabases);
  retryBootstrap.addEventListener('click', bootstrap);

  document.addEventListener('keydown', (event) => {
    if (event.key === 'Tab') {
      const visible = MODAL_IDS.map(byId).filter((modal) => !modal.hidden);
      const modal = visible[visible.length - 1];
      if (!modal) return;
      const focusable = Array.from(modal.querySelectorAll('button, input, select, textarea, a[href], [tabindex]'))
        .filter((node) => !node.disabled && node.tabIndex >= 0 && node.getClientRects().length);
      if (!focusable.length) { event.preventDefault(); return; }
      const first = focusable[0], last = focusable[focusable.length - 1];
      if (event.shiftKey && (document.activeElement === first || !modal.contains(document.activeElement))) {
        event.preventDefault(); last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !modal.contains(document.activeElement))) {
        event.preventDefault(); first.focus();
      }
      return;
    }
    if (event.key !== 'Escape') return;
    for (const id of MODAL_IDS) {
      if (!byId(id).hidden) {
        event.preventDefault();
        if (id === 'object-editor-modal') closeObjectEditor();
        else closeModal(id);
        if (id === 'row-editor-modal') rowEditor = null;
        if (id === 'import-modal') importState = null;
        return;
      }
    }
  });

  bootstrap();
})();
