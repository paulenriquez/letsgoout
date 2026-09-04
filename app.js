'use strict';

const dateCatalog = [
    { id: 'pizza', label: 'Pizza', emoji: '🍕' },
    { id: 'ramen', label: 'Ramen', emoji: '🍜' },
    { id: 'coffee', label: 'Coffee', emoji: '☕' },
    { id: 'drinks', label: 'Drinks', emoji: '🥂' },
    { id: 'steak', label: 'Steak', emoji: '🥩' },
    { id: 'gym', label: 'Gym', emoji: '🏋️' },
    { id: 'walk', label: 'Walk', emoji: '👟' },
    { id: 'run', label: 'Run', emoji: '🏃' },
    { id: 'any', label: 'Surprise Me!', emoji: '🎁' }
];
const ideaByID = new Map(dateCatalog.map((item) => [item.id, item]));
const selectedIdeas = new Set();
const recipientSelectedIdeas = new Set();
let activeInviteData = null;
let currentInvite = null;
let currentInviteToken = '';
let currentStatusToken = '';
let previewMode = false;

const byID = (id) => document.getElementById(id);
const allScreens = ['landing-page', 'asker-card', 'share-card', 'recipient-card', 'status-card', 'unavailable-card'];

function showScreen(id) {
    allScreens.forEach((screenID) => byID(screenID).classList.toggle('hidden', screenID !== id));
    window.scrollTo(0, 0);
}

function showError(id, message) {
    const el = byID(id);
    el.textContent = message;
    el.classList.toggle('hidden', !message);
}

function makeElement(tag, className, text) {
    const el = document.createElement(tag);
    if (className) el.className = className;
    if (text !== undefined) el.textContent = text;
    return el;
}

class APIError extends Error {
    constructor(status, message) {
        super(message);
        this.status = status;
    }
}

async function postJSON(endpoint, body) {
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 8000);
    try {
        const response = await fetch(endpoint, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
            signal: controller.signal,
            cache: 'no-store'
        });
        if (response.status === 204) return null;
        let data = null;
        try { data = await response.json(); } catch (_) { throw new APIError(response.status, 'The server returned an invalid response.'); }
        if (!response.ok) {
            const message = data && typeof data.error === 'string' ? data.error : 'Request failed.';
            throw new APIError(response.status, message);
        }
        return data;
    } catch (error) {
        if (error.name === 'AbortError') throw new APIError(0, 'The request timed out. Please try again.');
        throw error;
    } finally {
        window.clearTimeout(timeout);
    }
}

function isStringArray(value, min, max) {
    return Array.isArray(value) && value.length >= min && value.length <= max && value.every((item) => typeof item === 'string');
}

function isInviteResponse(data) {
    return data && typeof data.asker_name === 'string' && typeof data.recipient_name === 'string' &&
        ['her', 'him', 'them'].includes(data.pronoun) && isStringArray(data.offered_ideas, 1, 9) &&
        data.offered_ideas.every((id) => ideaByID.has(id)) && isStringArray(data.proposed_slots, 1, 5) &&
        data.proposed_slots.every((slot) => !Number.isNaN(Date.parse(slot))) && typeof data.expires_at === 'string';
}

function isCreateResponse(data) {
    return data && typeof data.invite_url === 'string' && typeof data.status_url === 'string' &&
        typeof data.expires_at === 'string' && data.invite_url.startsWith(`${location.origin}/#/invite/`) &&
        data.status_url.startsWith(`${location.origin}/#/status/`);
}

function isStatusResponse(data) {
    if (!data || !['pending', 'accepted'].includes(data.status) || typeof data.asker_name !== 'string' ||
        typeof data.recipient_name !== 'string' || !isStringArray(data.proposed_slots, 1, 5) || typeof data.expires_at !== 'string') return false;
    if (data.status === 'pending') return true;
    return typeof data.accepted_at === 'string' && Array.isArray(data.selected_ideas) &&
        data.selected_ideas.every((id) => ideaByID.has(id)) && typeof data.custom_idea === 'string' &&
        Number.isInteger(data.selected_slot_index) && data.selected_slot_index >= 0 &&
        data.selected_slot_index < data.proposed_slots.length;
}

function createIdeaCard(item, onClick, includeCheck = false) {
    const card = makeElement('div', 'idea-card');
    card.tabIndex = 0;
    card.setAttribute('role', 'button');
    if (includeCheck) card.appendChild(makeElement('div', 'badge-check', '✓'));
    card.appendChild(makeElement('div', 'emoji-icon', item.emoji));
    card.appendChild(makeElement('span', '', item.label));
    const activate = () => onClick(card);
    card.addEventListener('click', activate);
    card.addEventListener('keydown', (event) => {
        if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); activate(); }
    });
    return card;
}

function setupCreatorIdeas() {
    const wrapper = byID('ideas-wrapper');
    dateCatalog.forEach((item) => {
        wrapper.appendChild(createIdeaCard(item, (card) => {
            if (selectedIdeas.has(item.id)) selectedIdeas.delete(item.id); else selectedIdeas.add(item.id);
            card.classList.toggle('selected', selectedIdeas.has(item.id));
        }));
    });
}

function localDateKey(date) {
    const year = date.getFullYear();
    const month = String(date.getMonth() + 1).padStart(2, '0');
    const day = String(date.getDate()).padStart(2, '0');
    return `${year}-${month}-${day}`;
}

function formatTimeLabel(hour, minute) {
    const ampm = hour >= 12 ? 'PM' : 'AM';
    const displayHour = hour > 12 ? hour - 12 : hour === 0 ? 12 : hour;
    return `${displayHour}:${minute === 0 ? '00' : '30'} ${ampm}`;
}

function buildDateTimeData() {
    const dataset = [];
    const now = new Date();
    for (let i = 0; i < 8; i += 1) {
        const targetDate = new Date();
        targetDate.setDate(targetDate.getDate() + i);
        const times = [];
        for (let hour = 8; hour <= 22; hour += 1) {
            [0, 30].forEach((minute) => {
                const timestamp = new Date(targetDate);
                timestamp.setHours(hour, minute, 0, 0);
                if (timestamp > now && timestamp <= new Date(now.getTime() + 8 * 24 * 60 * 60 * 1000)) {
                    times.push({ value: `${String(hour).padStart(2, '0')}:${String(minute).padStart(2, '0')}`, label: formatTimeLabel(hour, minute) });
                }
            });
        }
        if (times.length) {
            let label = targetDate.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' });
            if (i === 0) label = `Today (${label})`;
            if (i === 1) label = `Tomorrow (${label})`;
            dataset.push({ value: localDateKey(targetDate), label, times });
        }
    }
    return dataset;
}

function appendOption(select, value, label) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = label;
    select.appendChild(option);
}

function createCustomPickerRow() {
    const slotsWrapper = byID('slots-wrapper');
    if (slotsWrapper.children.length >= 5) return;
    const dateTimeData = buildDateTimeData();
    if (!dateTimeData.length) return;
    const row = makeElement('div', 'custom-slot-row');
    const dateWrapper = makeElement('div', 'select-wrapper');
    const dateSelect = makeElement('select', 'slot-date-select');
    dateTimeData.forEach((item) => appendOption(dateSelect, item.value, item.label));
    dateWrapper.appendChild(dateSelect);
    const timeWrapper = makeElement('div', 'select-wrapper');
    const timeSelect = makeElement('select', 'slot-time-select');
    timeWrapper.appendChild(timeSelect);
    function syncTimes() {
        timeSelect.replaceChildren();
        const match = dateTimeData.find((item) => item.value === dateSelect.value);
        if (match) match.times.forEach((time) => appendOption(timeSelect, time.value, time.label));
    }
    dateSelect.addEventListener('change', syncTimes);
    syncTimes();
    const selectors = makeElement('div', 'slot-selectors');
    selectors.append(dateWrapper, timeWrapper);
    row.appendChild(selectors);
    slotsWrapper.appendChild(row);
    byID('add-slot-trigger').disabled = slotsWrapper.children.length >= 5;
}

function collectSlots() {
    return [...document.querySelectorAll('.custom-slot-row')].map((row) => {
        const date = row.querySelector('.slot-date-select').value;
        const time = row.querySelector('.slot-time-select').value;
        return new Date(`${date}T${time}:00`).toISOString();
    });
}

function formatSlot(slot) {
    return new Date(slot).toLocaleString([], { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
}

async function createInvite() {
    const button = byID('generate-btn');
    showError('create-error', '');
    const request = {
        asker_name: byID('asker-name').value.trim(),
        recipient_name: byID('recipient-name').value.trim(),
        pronoun: byID('recipient-pronoun').value,
        offered_ideas: [...selectedIdeas],
        proposed_slots: collectSlots()
    };
    if (!request.asker_name || !request.recipient_name || request.offered_ideas.length === 0) {
        showError('create-error', 'Add both names and pick at least one date idea.');
        return;
    }
    button.disabled = true;
    button.textContent = 'Generating links… ⏳';
    try {
        const result = await postJSON('/api/invites', request);
        if (!isCreateResponse(result)) throw new APIError(0, 'The server returned an invalid response.');
        activeInviteData = { ...request, expires_at: result.expires_at };
        byID('generated-invite-url').textContent = result.invite_url;
        byID('generated-status-url').textContent = result.status_url;
        showScreen('share-card');
    } catch (error) {
        showError('create-error', error.message || 'Could not generate the links. Please try again.');
    } finally {
        button.disabled = false;
        button.textContent = 'Generate Invite Links 🔗';
    }
}

async function copyLink(sourceID, buttonID, defaultText) {
    const button = byID(buttonID);
    try {
        await navigator.clipboard.writeText(byID(sourceID).textContent);
        button.textContent = 'Copied! ✔️';
        window.setTimeout(() => { button.textContent = defaultText; }, 2000);
    } catch (_) {
        button.textContent = 'Select and copy the link above';
        window.setTimeout(() => { button.textContent = defaultText; }, 2500);
    }
}

function updateAcceptButton() {
    const pickedTime = document.querySelector('input[name="time-radio"]:checked');
    const hasOffered = [...recipientSelectedIdeas].some((id) => id !== 'other');
    const hasCustom = recipientSelectedIdeas.has('other') && byID('other-freeform').value.trim().length > 0;
    byID('yes-btn').disabled = !pickedTime || (!hasOffered && !hasCustom);
}

function renderRecipientView(data, isPreview) {
    currentInvite = data;
    previewMode = isPreview;
    recipientSelectedIdeas.clear();
    byID('recipient-emoji').textContent = '✨';
    byID('recipient-title').textContent = `Hey ${data.recipient_name}! 💕`;
    const objectivePronoun = data.pronoun === 'him' ? 'him' : data.pronoun === 'them' ? 'them' : 'her';
	byID('recipient-subtitle').textContent = `${data.asker_name} wants to take you out! Customize your ideal date with ${objectivePronoun}:`;
	byID('other-freeform').value = '';
	showError('accept-error', '');
	document.querySelectorAll('.recipient-form-part').forEach((el) => el.classList.remove('hidden'));
	byID('other-input-container').classList.add('hidden');

    const ideasGrid = byID('recipient-ideas-grid');
    ideasGrid.replaceChildren();
    data.offered_ideas.forEach((id) => {
        const item = ideaByID.get(id);
        ideasGrid.appendChild(createIdeaCard(item, (card) => {
            if (recipientSelectedIdeas.has(id)) recipientSelectedIdeas.delete(id); else recipientSelectedIdeas.add(id);
            card.classList.toggle('selected', recipientSelectedIdeas.has(id));
            updateAcceptButton();
        }, true));
    });
    const other = { id: 'other', label: 'Other...', emoji: '🤔' };
    ideasGrid.appendChild(createIdeaCard(other, (card) => {
        if (recipientSelectedIdeas.has('other')) recipientSelectedIdeas.delete('other'); else recipientSelectedIdeas.add('other');
        const selected = recipientSelectedIdeas.has('other');
        card.classList.toggle('selected', selected);
        byID('other-input-container').classList.toggle('hidden', !selected);
        updateAcceptButton();
    }, true));

    const slotsContainer = byID('slots-selector-container');
    slotsContainer.replaceChildren();
    data.proposed_slots.forEach((slot, index) => {
        const label = makeElement('label', 'select-item');
        const radio = document.createElement('input');
        radio.type = 'radio'; radio.name = 'time-radio'; radio.value = String(index);
        label.append(radio, makeElement('span', '', formatSlot(slot)));
        radio.addEventListener('change', () => {
            slotsContainer.querySelectorAll('.select-item').forEach((el) => el.classList.remove('selected'));
            label.classList.add('selected');
            updateAcceptButton();
        });
        slotsContainer.appendChild(label);
    });
    byID('preview-back-btn').classList.toggle('hidden', !isPreview);
    prepareNoButtonPosition();
    showScreen('recipient-card');
    updateAcceptButton();
    window.requestAnimationFrame(setupInitialNoButtonPosition);
}

function renderAccepted(selectedLabels, customIdea, slotLabel) {
    byID('recipient-emoji').textContent = '🎉✨';
    byID('recipient-title').textContent = "It's a date!!";
    const subtitle = byID('recipient-subtitle');
    subtitle.replaceChildren();
    subtitle.append('Going for: ', makeElement('strong', '', [...selectedLabels, ...(customIdea ? [customIdea] : [])].join(' & ')), document.createElement('br'));
    subtitle.append('Scheduled for: ', makeElement('strong', '', slotLabel), '.', document.createElement('br'), document.createElement('br'), "Can't wait! 🍰🌸");
    document.querySelectorAll('.recipient-form-part').forEach((el) => el.classList.add('hidden'));
    showError('accept-error', '');
    window.scrollTo(0, 0);
}

async function acceptInvite() {
    const selectedTime = document.querySelector('input[name="time-radio"]:checked');
    if (!selectedTime || byID('yes-btn').disabled) return;
    const selectedIDs = [...recipientSelectedIdeas].filter((id) => id !== 'other');
    const customIdea = recipientSelectedIdeas.has('other') ? byID('other-freeform').value.trim() : '';
    const slotIndex = Number(selectedTime.value);
    const labels = selectedIDs.map((id) => ideaByID.get(id).label);
    if (previewMode) {
        renderAccepted(labels, customIdea, formatSlot(currentInvite.proposed_slots[slotIndex]));
        return;
    }
    const button = byID('yes-btn');
    button.disabled = true;
    showError('accept-error', '');
    try {
        const result = await postJSON('/api/invites/accept', {
            token: currentInviteToken,
            selected_ideas: selectedIDs,
            custom_idea: customIdea,
            selected_slot_index: slotIndex
        });
        if (!result || result.status !== 'accepted' || typeof result.expires_at !== 'string') throw new APIError(0, 'The server returned an invalid response.');
        renderAccepted(labels, customIdea, formatSlot(currentInvite.proposed_slots[slotIndex]));
    } catch (error) {
        showError('accept-error', error.status === 409 ? 'This invitation has already been accepted.' : (error.message || 'Could not save your answer.'));
        updateAcceptButton();
    }
}

function addStatusRow(parent, label, value) {
    const row = makeElement('div', 'status-row');
    row.append(makeElement('strong', '', `${label}: `), document.createTextNode(value));
    parent.appendChild(row);
}

function renderStatus(data) {
    const details = byID('status-details');
    details.replaceChildren();
    byID('status-title').textContent = `${data.recipient_name}'s Invite`;
    if (data.status === 'pending') {
        byID('status-emoji').textContent = '💌';
        byID('status-summary').textContent = 'Still waiting for a response.';
        addStatusRow(details, 'Expires', new Date(data.expires_at).toLocaleString());
    } else {
        byID('status-emoji').textContent = '🎉✨';
        byID('status-summary').textContent = "It's a date! Here's the accepted plan:";
        const labels = data.selected_ideas.map((id) => ideaByID.get(id).label);
        if (data.custom_idea) labels.push(data.custom_idea);
        addStatusRow(details, 'Vibe', labels.join(' & '));
        addStatusRow(details, 'Time', formatSlot(data.proposed_slots[data.selected_slot_index]));
        addStatusRow(details, 'Accepted', new Date(data.accepted_at).toLocaleString());
        addStatusRow(details, 'Expires', new Date(data.expires_at).toLocaleString());
    }
    details.classList.remove('hidden');
    byID('status-updated').textContent = `Last checked ${new Date().toLocaleTimeString()}`;
    showError('status-error', '');
}

async function refreshStatus() {
    if (!currentStatusToken || document.hidden || !navigator.onLine) return;
    const button = byID('status-refresh-btn');
    button.disabled = true;
    try {
        const data = await postJSON('/api/status/view', { token: currentStatusToken });
        if (!isStatusResponse(data)) throw new APIError(0, 'The server returned an invalid response.');
        renderStatus(data);
    } catch (error) {
        showError('status-error', error.status === 404 ? 'This invitation is unavailable or has expired.' : (error.message || 'Could not refresh the status.'));
    } finally {
        button.disabled = false;
    }
}

async function deleteInvite() {
    if (!window.confirm('Permanently delete this invitation? Both links will stop working, and this cannot be undone.')) return;
    const button = byID('status-delete-btn');
    button.disabled = true;
    try {
        await postJSON('/api/status/delete', { token: currentStatusToken });
        currentStatusToken = '';
        byID('status-emoji').textContent = '🗑️';
        byID('status-title').textContent = 'Invite Deleted';
        byID('status-summary').textContent = 'Both invitation links have been permanently deleted.';
        byID('status-details').classList.add('hidden');
        byID('status-updated').textContent = '';
        byID('status-refresh-btn').classList.add('hidden');
        button.classList.add('hidden');
        showError('status-error', '');
    } catch (error) {
        showError('status-error', error.status === 404 ? 'This invitation is unavailable or has expired.' : (error.message || 'Could not delete the invitation.'));
        button.disabled = false;
    }
}

async function loadInvite(token) {
    currentInviteToken = token;
    try {
        const data = await postJSON('/api/invites/view', { token });
        if (!isInviteResponse(data)) throw new APIError(0, 'The server returned an invalid response.');
        renderRecipientView(data, false);
    } catch (error) {
        byID('unavailable-card').querySelector('p').textContent = error.status === 404
            ? 'This invite link does not exist, has expired, or was deleted.'
            : 'This invitation could not be loaded. Please try again.';
        showScreen('unavailable-card');
    }
}

function routeFromHash() {
    const inviteMatch = location.hash.match(/^#\/invite\/([A-Za-z0-9_-]{22})$/);
    const statusMatch = location.hash.match(/^#\/status\/([A-Za-z0-9_-]{22})$/);
    if (inviteMatch) { currentStatusToken = ''; loadInvite(inviteMatch[1]); return; }
    if (statusMatch) {
        currentInviteToken = '';
        currentStatusToken = statusMatch[1];
        showScreen('status-card');
        refreshStatus();
        return;
    }
    currentInviteToken = '';
    currentStatusToken = '';
    showScreen('landing-page');
}

function setupNoButton() {
    const noButton = byID('no-btn');
    function dodge() {
        if (noButton.style.position !== 'fixed') setupInitialNoButtonPosition();
        noButton.getBoundingClientRect();
        noButton.classList.add('dodge-ready');
        const padding = 30;
        const maxX = Math.max(padding, window.innerWidth - noButton.offsetWidth - padding);
        const maxY = Math.max(padding, window.innerHeight - noButton.offsetHeight - padding);
        noButton.style.left = `${Math.floor(Math.random() * (maxX - padding + 1) + padding)}px`;
        noButton.style.top = `${Math.floor(Math.random() * (maxY - padding + 1) + padding)}px`;
    }
    noButton.addEventListener('mouseover', dodge);
    noButton.addEventListener('touchstart', (event) => { event.preventDefault(); dodge(); }, { passive: false });
    window.addEventListener('resize', setupInitialNoButtonPosition);
}

function setupInitialNoButtonPosition() {
    const card = byID('recipient-card');
    const noButton = byID('no-btn');
    if (card.classList.contains('hidden')) return;
    prepareNoButtonPosition();
    const rect = noButton.getBoundingClientRect();
    noButton.style.left = `${rect.left}px`;
    noButton.style.top = `${rect.top}px`;
    noButton.style.position = 'fixed';
}

function prepareNoButtonPosition() {
    const noButton = byID('no-btn');
    noButton.classList.remove('dodge-ready');
    noButton.style.position = '';
    noButton.style.left = '';
    noButton.style.top = '';
}

function setupInstallPrompt() {
    const overlay = byID('install-modal-overlay');
    const acceptButton = byID('install-accept-btn');
    let deferredPrompt = null;
    const ua = navigator.userAgent || '';
    const standalone = window.matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
    const ios = /iphone|ipad|ipod/i.test(ua) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
    const inApp = /Instagram|FBAN|FBAV|FB_IAB|Messenger|Line\/|Twitter|TikTok|Snapchat|WhatsApp/i.test(ua);
    const dismissed = localStorage.getItem('installPromptDismissed') === '1';
    const eligible = !standalone && !location.hash.startsWith('#/invite/') && !location.hash.startsWith('#/status/') && !dismissed;
    const show = () => { if (eligible) overlay.classList.remove('hidden'); };
    if (eligible && inApp) {
        byID('install-modal-title').textContent = 'Open in your browser';
        byID('install-modal-text').textContent = 'To save this app, use the ••• menu and choose “Open in Browser” first 💕';
        acceptButton.classList.add('hidden');
        window.setTimeout(show, 1800);
    } else if (eligible && ios) {
        byID('install-modal-title').textContent = /CriOS|FxiOS|EdgiOS/i.test(ua) ? 'Open in Safari' : 'Add to Home Screen';
        byID('install-modal-text').textContent = 'In Safari, tap Share, then choose “Add to Home Screen” 💕';
        acceptButton.classList.add('hidden');
        window.setTimeout(show, 1500);
    } else if (eligible) {
        window.addEventListener('beforeinstallprompt', (event) => {
            event.preventDefault(); deferredPrompt = event; show();
        });
    }
    acceptButton.addEventListener('click', async () => {
        if (deferredPrompt) { deferredPrompt.prompt(); await deferredPrompt.userChoice; deferredPrompt = null; }
        overlay.classList.add('hidden');
    });
    byID('install-dismiss-btn').addEventListener('click', () => {
        localStorage.setItem('installPromptDismissed', '1'); overlay.classList.add('hidden');
    });
    window.addEventListener('appinstalled', () => overlay.classList.add('hidden'));
}

document.addEventListener('DOMContentLoaded', () => {
    if ('serviceWorker' in navigator) navigator.serviceWorker.register('/service-worker.js').catch(() => {});
    setupCreatorIdeas();
    createCustomPickerRow();
    setupNoButton();
    setupInstallPrompt();
    byID('start-btn').addEventListener('click', () => showScreen('asker-card'));
    byID('add-slot-trigger').addEventListener('click', createCustomPickerRow);
    byID('generate-btn').addEventListener('click', createInvite);
    byID('copy-invite-btn').addEventListener('click', () => copyLink('generated-invite-url', 'copy-invite-btn', 'Copy Invite Link 📋'));
    byID('copy-status-btn').addEventListener('click', () => copyLink('generated-status-url', 'copy-status-btn', 'Copy Status Link 🔒'));
    byID('preview-btn').addEventListener('click', () => renderRecipientView(activeInviteData, true));
    byID('share-back-btn').addEventListener('click', () => showScreen('asker-card'));
    byID('preview-back-btn').addEventListener('click', () => showScreen('share-card'));
    byID('other-freeform').addEventListener('input', updateAcceptButton);
    byID('yes-btn').addEventListener('click', acceptInvite);
    byID('status-refresh-btn').addEventListener('click', refreshStatus);
    byID('status-delete-btn').addEventListener('click', deleteInvite);
    byID('unavailable-create-btn').addEventListener('click', () => {
        currentInviteToken = '';
        currentStatusToken = '';
        history.replaceState(null, '', `${location.pathname}${location.search}`);
        showScreen('asker-card');
    });
    document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshStatus(); });
    window.addEventListener('online', refreshStatus);
    window.setInterval(refreshStatus, 15000);
    window.addEventListener('hashchange', routeFromHash);
    routeFromHash();
});
