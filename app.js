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
const customIdeaEmojis = ['🍕', '🍜', '☕', '🥂', '🥩', '🏋️', '👟', '🏃', '🎁', '🎬', '🎨', '🎳', '🎤', '🎮', '🧺', '🌅', '🏖️', '🏛️', '📚', '🍦', '🧋', '🍣', '🌮', '🚲', '🧗', '🎭', '🎡', '🌿', '✨'];
const ideaByID = new Map(dateCatalog.map((item) => [item.id, item]));
const selectedIdeas = new Set();
const recipientSelectedIdeas = new Set();
let activeInviteData = null;
let activeCreateRequest = null;
let currentInvite = null;
let currentInviteToken = '';
let currentStatusToken = '';
let currentStatusInviteURL = '';
let previewMode = false;
let previewSource = '';
let celebrationTimer = 0;
let statusRefreshTimer = 0;
let statusCountdownTimer = 0;
let nextStatusRefreshAt = 0;
let statusAutoRefreshEnabled = false;

const celebrationEmojis = ['😍'];
const statusRefreshInterval = 15000;

const byID = (id) => document.getElementById(id);
const allScreens = ['landing-page', 'asker-card', 'share-card', 'recipient-card', 'status-card', 'unavailable-card'];

function cleanupLegacyPWA() {
    if ('serviceWorker' in navigator) {
        navigator.serviceWorker.getRegistrations()
            .then((registrations) => Promise.all(registrations.map((registration) => registration.unregister())))
            .catch(() => {});
    }
    if ('caches' in window) {
        window.caches.keys()
            .then((keys) => Promise.all(keys.filter((key) => key.startsWith('letsgoout-')).map((key) => window.caches.delete(key))))
            .catch(() => {});
    }
}

cleanupLegacyPWA();

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

function isCustomIdea(item, requireID) {
    return item && (!requireID || typeof item.id === 'string') && customIdeaEmojis.includes(item.emoji) &&
        typeof item.title === 'string' && item.title.length > 0 && [...item.title].length <= 60;
}

function isInviteResponse(data) {
    if (!data || !['pending', 'accepted'].includes(data.status) || typeof data.asker_name !== 'string' || typeof data.recipient_name !== 'string' ||
        !isStringArray(data.offered_ideas, 0, 9) || !data.offered_ideas.every((id) => ideaByID.has(id)) ||
        !Array.isArray(data.custom_ideas) || !data.custom_ideas.every((item) => isCustomIdea(item, true)) ||
        data.offered_ideas.length + data.custom_ideas.length === 0 || typeof data.sender_message !== 'string' ||
        [...data.sender_message].length > 280 || !isStringArray(data.proposed_slots, 1, 5) ||
        !data.proposed_slots.every((slot) => !Number.isNaN(Date.parse(slot))) || typeof data.expires_at !== 'string') return false;
    if (data.status === 'pending') return true;
    const customIDs = new Set(data.custom_ideas.map((item) => item.id));
    return Array.isArray(data.selected_ideas) &&
        data.selected_ideas.every((id) => ideaByID.has(id) || customIDs.has(id)) &&
        typeof data.custom_idea === 'string' && [...data.custom_idea].length <= 120 &&
        (data.selected_ideas.length > 0 || data.custom_idea.length > 0) &&
        typeof data.recipient_message === 'string' && [...data.recipient_message].length <= 280 &&
        Number.isInteger(data.selected_slot_index) && data.selected_slot_index >= 0 &&
        data.selected_slot_index < data.proposed_slots.length;
}

function isCreateResponse(data) {
    return data && typeof data.invite_url === 'string' && typeof data.status_url === 'string' &&
        typeof data.expires_at === 'string' && data.invite_url.startsWith(`${location.origin}/#/invite/`) &&
        data.status_url.startsWith(`${location.origin}/#/status/`);
}

function isStatusResponse(data) {
    if (!data || !['pending', 'accepted'].includes(data.status) || typeof data.asker_name !== 'string' ||
        typeof data.recipient_name !== 'string' || !Array.isArray(data.custom_ideas) ||
        !data.custom_ideas.every((item) => isCustomIdea(item, true)) ||
        !isStringArray(data.proposed_slots, 1, 5) || typeof data.expires_at !== 'string' ||
        (data.invite_url !== undefined && (typeof data.invite_url !== 'string' ||
            !data.invite_url.startsWith(`${location.origin}/#/invite/`)))) return false;
    if (data.status === 'pending') return true;
    const customIDs = new Set(data.custom_ideas.map((item) => item.id));
    return typeof data.accepted_at === 'string' && Array.isArray(data.selected_ideas) &&
        data.selected_ideas.every((id) => ideaByID.has(id) || customIDs.has(id)) && typeof data.custom_idea === 'string' &&
        typeof data.recipient_message === 'string' && [...data.recipient_message].length <= 280 &&
        Number.isInteger(data.selected_slot_index) && data.selected_slot_index >= 0 &&
        data.selected_slot_index < data.proposed_slots.length;
}

function createIdeaCard(item, onClick, includeCheck = false) {
    const card = makeElement('div', 'idea-card');
    if (includeCheck) card.appendChild(makeElement('div', 'badge-check', '✓'));
    card.appendChild(makeElement('div', 'emoji-icon', item.emoji));
    card.appendChild(makeElement('span', '', item.label || item.title));
    if (!onClick) {
        card.classList.add('preview-idea-card');
        return card;
    }
    card.tabIndex = 0;
    card.setAttribute('role', 'button');
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
        const label = makeElement('label', 'creator-idea-option');
        const checkbox = document.createElement('input');
        checkbox.type = 'checkbox';
        checkbox.value = item.id;
        checkbox.addEventListener('change', () => {
            if (checkbox.checked) selectedIdeas.add(item.id); else selectedIdeas.delete(item.id);
        });
        label.append(checkbox, makeElement('span', 'creator-idea-emoji', item.emoji), makeElement('span', 'creator-idea-title', item.label));
        wrapper.appendChild(label);
    });
}

function closeEmojiPalettes(except) {
    document.querySelectorAll('.emoji-palette').forEach((palette) => {
        if (palette !== except) {
            palette.classList.add('hidden');
            palette.parentElement.querySelector('.emoji-selector-button').setAttribute('aria-expanded', 'false');
        }
    });
}

function createCustomIdeaRow() {
    const wrapper = byID('custom-ideas-wrapper');
    const row = makeElement('div', 'custom-idea-row');
    const emojiButton = makeElement('button', 'emoji-selector-button', '✨');
    emojiButton.type = 'button';
    emojiButton.setAttribute('aria-label', 'Choose date idea emoji');
    emojiButton.setAttribute('aria-expanded', 'false');
    emojiButton.dataset.emoji = '✨';
    const title = document.createElement('input');
    title.type = 'text';
    title.maxLength = 60;
    title.placeholder = 'Date idea title';
    title.setAttribute('aria-label', 'Custom date idea title');
    const remove = makeElement('button', 'remove-custom-idea', '×');
    remove.type = 'button';
    remove.setAttribute('aria-label', 'Remove custom date idea');
    const palette = makeElement('div', 'emoji-palette hidden');
    palette.setAttribute('role', 'listbox');
    customIdeaEmojis.forEach((emoji) => {
        const choice = makeElement('button', 'emoji-choice', emoji);
        choice.type = 'button';
        choice.setAttribute('aria-label', `Use ${emoji}`);
        choice.addEventListener('click', (event) => {
            event.stopPropagation();
            emojiButton.textContent = emoji;
            emojiButton.dataset.emoji = emoji;
            palette.classList.add('hidden');
            emojiButton.setAttribute('aria-expanded', 'false');
            title.focus();
        });
        palette.appendChild(choice);
    });
    emojiButton.addEventListener('click', (event) => {
        event.stopPropagation();
        const willOpen = palette.classList.contains('hidden');
        closeEmojiPalettes(palette);
        palette.classList.toggle('hidden', !willOpen);
        emojiButton.setAttribute('aria-expanded', String(willOpen));
    });
    remove.addEventListener('click', () => row.remove());
    row.append(emojiButton, title, remove, palette);
    wrapper.appendChild(row);
    title.focus();
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

function updateSlotControls() {
    const rows = [...byID('slots-wrapper').querySelectorAll('.custom-slot-row')];
    const canRemove = rows.length > 1;
    rows.forEach((row, index) => {
        const remove = row.querySelector('.remove-slot-btn');
        row.classList.toggle('has-remove', canRemove);
        remove.classList.toggle('hidden', !canRemove);
        remove.setAttribute('aria-label', `Remove date and time option ${index + 1}`);
    });
    byID('add-slot-trigger').disabled = rows.length >= 5;
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
    const remove = makeElement('button', 'remove-slot-btn', '×');
    remove.type = 'button';
    remove.addEventListener('click', () => {
        if (slotsWrapper.children.length <= 1) return;
        row.remove();
        updateSlotControls();
    });
    row.append(selectors, remove);
    slotsWrapper.appendChild(row);
    updateSlotControls();
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

function formatStatusDate(value) {
    const parts = Object.fromEntries(new Intl.DateTimeFormat('en-US', {
        day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit',
        second: '2-digit', hour12: true
    }).formatToParts(new Date(value)).map((part) => [part.type, part.value]));
    return `${parts.day} ${parts.month} ${parts.year}, ${parts.hour}:${parts.minute}:${parts.second} ${parts.dayPeriod.toUpperCase()}`;
}

function buildInviteDraft() {
    showError('create-error', '');
    const customIdeas = [...document.querySelectorAll('.custom-idea-row')].map((row) => ({
        emoji: row.querySelector('.emoji-selector-button').dataset.emoji,
        title: row.querySelector('input[type="text"]').value.trim()
    }));
    const request = {
        asker_name: byID('asker-name').value.trim(),
        recipient_name: byID('recipient-name').value.trim(),
        offered_ideas: [...selectedIdeas],
        custom_ideas: customIdeas,
        sender_message: byID('sender-message').value.trim(),
        proposed_slots: collectSlots()
    };
    if (!request.asker_name || !request.recipient_name) {
        throw new Error('Add both names before previewing the invite.');
    }
    if (customIdeas.some((idea) => !idea.title)) {
        throw new Error('Add a title to every custom date idea or remove the unfinished row.');
    }
    const normalizedTitles = customIdeas.map((idea) => idea.title.toLocaleLowerCase());
    if (new Set(normalizedTitles).size !== normalizedTitles.length) {
        throw new Error('Custom date idea titles must be unique.');
    }
    if (request.offered_ideas.length + customIdeas.length === 0) {
        throw new Error('Pick or add at least one date idea.');
    }
    if ([...request.sender_message].length > 280) {
        throw new Error('Your personal message must not exceed 280 characters.');
    }
    if (new Set(request.proposed_slots).size !== request.proposed_slots.length) {
        throw new Error('Choose different date and time options.');
    }
    if (new TextEncoder().encode(JSON.stringify(request)).length > 8 * 1024) {
        throw new Error('This invite is too large. Remove some custom date ideas or shorten their titles.');
    }
    const preview = {
        ...request,
        custom_ideas: customIdeas.map((idea, index) => ({ id: `custom:${index}`, ...idea }))
    };
    return { request, preview };
}

function previewDraft() {
    try {
        const draft = buildInviteDraft();
        activeCreateRequest = draft.request;
        activeInviteData = draft.preview;
        renderRecipientView(activeInviteData, 'draft');
    } catch (error) {
        showError('create-error', error.message);
    }
}

async function createInvite() {
    if (!activeCreateRequest || previewSource !== 'draft') return;
    const button = byID('preview-generate-btn');
    showError('preview-error', '');
    button.disabled = true;
    button.textContent = 'Generating links… ⏳';
    try {
        const result = await postJSON('/api/invites', activeCreateRequest);
        if (!isCreateResponse(result)) throw new APIError(0, 'The server returned an invalid response.');
        activeInviteData = { ...activeInviteData, expires_at: result.expires_at };
        const recipientName = activeCreateRequest.recipient_name;
        byID('share-instructions').textContent = `Share the invite link with ${recipientName}, and then use the private status link to view their response.`;
        byID('share-invite-label').textContent = `Share this to ${recipientName}`;
        byID('share-status-label').textContent = `Save this link to view ${recipientName}'s response`;
        byID('generated-invite-url').textContent = result.invite_url;
        byID('generated-status-url').textContent = result.status_url;
        showScreen('share-card');
    } catch (error) {
        showError('preview-error', error.message || 'Could not generate the links. Please try again.');
    } finally {
        button.disabled = false;
        button.textContent = 'Generate Invite Links 🔗';
    }
}

function startNewInvite() {
    byID('asker-name').value = '';
    byID('recipient-name').value = '';
    byID('sender-message').value = '';
    byID('sender-message-count').textContent = '0';
    byID('recipient-message').value = '';
    byID('recipient-message').disabled = false;
    byID('recipient-message-count').textContent = '0';
    byID('other-freeform').value = '';

    selectedIdeas.clear();
    byID('ideas-wrapper').querySelectorAll('input[type="checkbox"]').forEach((checkbox) => {
        checkbox.checked = false;
    });
    byID('custom-ideas-wrapper').replaceChildren();
    byID('slots-wrapper').replaceChildren();
    byID('add-slot-trigger').disabled = false;
    createCustomPickerRow();

    activeInviteData = null;
    activeCreateRequest = null;
    currentInvite = null;
    currentInviteToken = '';
    currentStatusToken = '';
    previewMode = false;
    previewSource = '';
    recipientSelectedIdeas.clear();
    prepareNoButtonPosition();

    byID('share-instructions').textContent = 'Share the invite link with them, and then use the private status link to view their response.';
    byID('share-invite-label').textContent = 'Share this';
    byID('share-status-label').textContent = 'Save this link to view their response';
    byID('generated-invite-url').textContent = '';
    byID('generated-status-url').textContent = '';
    showError('create-error', '');
    showError('preview-error', '');
    showError('accept-error', '');

    history.replaceState(null, '', location.pathname);
    showScreen('asker-card');
    byID('asker-name').focus();
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
    if (previewMode) {
        byID('yes-btn').disabled = true;
        return;
    }
    const pickedTime = document.querySelector('input[name="time-radio"]:checked');
    const otherSelected = recipientSelectedIdeas.has('other');
    const otherIncomplete = otherSelected && byID('other-freeform').value.trim().length === 0;
    byID('yes-btn').disabled = !pickedTime || recipientSelectedIdeas.size === 0 || otherIncomplete;
}

function clearCelebration() {
    window.clearTimeout(celebrationTimer);
    celebrationTimer = 0;
    byID('celebration-confetti').replaceChildren();
}

function startCelebration() {
    clearCelebration();
    if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

    const confetti = byID('celebration-confetti');
    const pieceCount = window.innerWidth < 480 ? 160 : 240;
    const waveSeconds = 1.2;
    const minimumFallSeconds = 3;
    const fallVarianceSeconds = 0.8;
    for (let index = 0; index < pieceCount; index += 1) {
        const piece = makeElement('span', 'confetti-piece', celebrationEmojis[index % celebrationEmojis.length]);
        const isHeroPiece = index % 5 === 0;
        const horizontalSlot = index % 14;
        const startX = -4 + horizontalSlot * (108 / 13) + (Math.random() * 4.8 - 2.4);
        let drift = Math.random() * 36 - 18;
        if (horizontalSlot < 3) drift = Math.random() * 18;
        if (horizontalSlot > 10) drift = Math.random() * -18;
        const duration = minimumFallSeconds + Math.random() * fallVarianceSeconds;
        const delay = -duration + ((index + Math.random()) / pieceCount) * (waveSeconds + duration);
        const size = isHeroPiece ? 4.5 + Math.random() * 1.5 : 2.25 + Math.random() * 1.75;
        piece.setAttribute('aria-hidden', 'true');
        piece.style.setProperty('--confetti-x', `${startX.toFixed(2)}vw`);
        piece.style.setProperty('--confetti-drift', `${drift.toFixed(2)}vw`);
        piece.style.setProperty('--confetti-spin', `${Math.round(Math.random() * 1080 - 540)}deg`);
        piece.style.setProperty('--confetti-delay', `${delay.toFixed(2)}s`);
        piece.style.setProperty('--confetti-duration', `${duration.toFixed(2)}s`);
        piece.style.setProperty('--confetti-size', `${size.toFixed(2)}rem`);
        piece.addEventListener('animationend', () => piece.remove(), { once: true });
        confetti.appendChild(piece);
    }
    const maximumFallSeconds = minimumFallSeconds + fallVarianceSeconds;
    celebrationTimer = window.setTimeout(clearCelebration, (waveSeconds + maximumFallSeconds + 0.1) * 1000);
}

function renderRecipientView(data, source = '') {
    currentInvite = data;
    previewMode = Boolean(source);
    previewSource = source;
    recipientSelectedIdeas.clear();
    const recipientCard = byID('recipient-card');
    clearCelebration();
    recipientCard.classList.remove('accepted-state');
    recipientCard.classList.toggle('preview-read-only', previewMode);
    byID('accepted-plan').classList.add('hidden');
    byID('accepted-ideas-icon').textContent = '🚀';
    byID('accepted-ideas').textContent = '';
    byID('accepted-slot').textContent = '';
    byID('accepted-message').classList.add('hidden');
    byID('accepted-message-label').textContent = 'Your message';
    byID('accepted-message-text').textContent = '';
    byID('recipient-emoji').textContent = '✨';
    byID('recipient-title').textContent = `Hey ${data.recipient_name}! 💕`;
    byID('recipient-subtitle').textContent = data.sender_message;
    byID('recipient-subtitle').classList.toggle('hidden', !data.sender_message);
    byID('recipient-message-label').textContent = `3. Message for ${data.asker_name} (Optional)`;
    byID('recipient-message').placeholder = `Write something for ${data.asker_name}…`;
    byID('recipient-message').value = '';
    byID('recipient-message').disabled = previewMode;
    byID('recipient-message-count').textContent = '0';
    byID('other-freeform').value = '';
    byID('other-freeform').setAttribute('aria-required', 'false');
    byID('other-freeform').setAttribute('aria-invalid', 'false');
    showError('accept-error', '');
    showError('preview-error', '');
    document.querySelectorAll('.recipient-form-part').forEach((el) => el.classList.remove('hidden'));
    byID('other-input-container').classList.add('hidden');

    const ideasGrid = byID('recipient-ideas-grid');
    ideasGrid.replaceChildren();
    data.offered_ideas.forEach((id) => {
        const item = ideaByID.get(id);
        const selectIdea = previewMode ? null : (card) => {
            if (recipientSelectedIdeas.has(id)) recipientSelectedIdeas.delete(id); else recipientSelectedIdeas.add(id);
            card.classList.toggle('selected', recipientSelectedIdeas.has(id));
            updateAcceptButton();
        };
        ideasGrid.appendChild(createIdeaCard(item, selectIdea, !previewMode));
    });
    data.custom_ideas.forEach((item) => {
        const selectIdea = previewMode ? null : (card) => {
            if (recipientSelectedIdeas.has(item.id)) recipientSelectedIdeas.delete(item.id); else recipientSelectedIdeas.add(item.id);
            card.classList.toggle('selected', recipientSelectedIdeas.has(item.id));
            updateAcceptButton();
        };
        ideasGrid.appendChild(createIdeaCard(item, selectIdea, !previewMode));
    });
    const other = { id: 'other', label: 'Other...', emoji: '🤔' };
    const selectOther = previewMode ? null : (card) => {
        if (recipientSelectedIdeas.has('other')) recipientSelectedIdeas.delete('other'); else recipientSelectedIdeas.add('other');
        const selected = recipientSelectedIdeas.has('other');
        card.classList.toggle('selected', selected);
        byID('other-input-container').classList.toggle('hidden', !selected);
        byID('other-freeform').setAttribute('aria-required', String(selected));
        if (!selected) byID('other-freeform').setAttribute('aria-invalid', 'false');
        prepareNoButtonPosition();
        updateAcceptButton();
    };
    ideasGrid.appendChild(createIdeaCard(other, selectOther, !previewMode));

    const slotsContainer = byID('slots-selector-container');
    slotsContainer.replaceChildren();
    data.proposed_slots.forEach((slot, index) => {
        const label = makeElement('label', 'select-item');
        const radio = document.createElement('input');
        radio.type = 'radio'; radio.name = 'time-radio'; radio.value = String(index);
        radio.disabled = previewMode;
        label.append(radio, makeElement('span', '', formatSlot(slot)));
        if (!previewMode) {
            radio.addEventListener('change', () => {
                slotsContainer.querySelectorAll('.select-item').forEach((el) => el.classList.remove('selected'));
                label.classList.add('selected');
                updateAcceptButton();
            });
        }
        slotsContainer.appendChild(label);
    });
    byID('preview-toolbar').classList.toggle('hidden', !previewMode);
    byID('preview-back-btn').textContent = source === 'draft' ? '← Back to Edit' : '← Back to Links';
    byID('preview-generate-btn').classList.toggle('hidden', source !== 'draft');
    byID('no-btn').disabled = previewMode;
    prepareNoButtonPosition();
    showScreen('recipient-card');
    updateAcceptButton();
}

function renderAccepted(selectedLabels, selectedEmojis, customIdea, slotLabel, recipientMessage) {
    const recipientCard = byID('recipient-card');
    previewMode = false;
    previewSource = '';
    recipientCard.classList.remove('preview-read-only');
    recipientCard.classList.add('accepted-state');
    byID('preview-toolbar').classList.add('hidden');
    prepareNoButtonPosition();
    const recipientEmoji = byID('recipient-emoji');
    recipientEmoji.replaceChildren(
        makeElement('span', 'accepted-popper', '🎉'),
        makeElement('span', 'accepted-sparkles', '✨'),
    );
    byID('recipient-title').textContent = `${currentInvite.recipient_name}, it’s a date!`;
    byID('recipient-subtitle').textContent = `Your response has been shared with ${currentInvite.asker_name}`;
    byID('recipient-subtitle').classList.remove('hidden');
    const acceptedLabels = [...selectedLabels, ...(customIdea ? [customIdea] : [])];
    const acceptedEmojis = [...selectedEmojis, ...(customIdea ? ['🤔'] : [])];
    renderPlanIdeas(byID('accepted-ideas-icon'), byID('accepted-ideas'), acceptedLabels, acceptedEmojis);
    byID('accepted-slot').textContent = slotLabel;
    byID('accepted-message-label').textContent = `Your message to ${currentInvite.asker_name}`;
    byID('accepted-message-text').textContent = recipientMessage;
    byID('accepted-message').classList.toggle('hidden', !recipientMessage);
    byID('accepted-plan').classList.remove('hidden');
    document.querySelectorAll('.recipient-form-part').forEach((el) => el.classList.add('hidden'));
    showError('accept-error', '');
    showScreen('recipient-card');
    startCelebration();
}

function renderPlanIdeas(icon, list, labels, emojis) {
    icon.textContent = emojis.length > 1 ? '🚀' : (emojis[0] || '🚀');
    const showItemEmojis = emojis.length > 1;
    list.replaceChildren();
    labels.forEach((label, index) => {
        const item = makeElement('li', showItemEmojis ? 'accepted-idea' : 'accepted-idea accepted-idea-single');
        if (showItemEmojis) item.appendChild(makeElement('span', 'accepted-idea-emoji', emojis[index]));
        item.appendChild(makeElement('span', 'accepted-idea-label', label));
        list.appendChild(item);
    });
}

function renderAcceptedResponse(data) {
    currentInvite = data;
    const labels = data.selected_ideas.map((id) => ideaLabel(id, data.custom_ideas));
    const emojis = data.selected_ideas.map((id) => ideaEmoji(id, data.custom_ideas));
    renderAccepted(labels, emojis, data.custom_idea, formatSlot(data.proposed_slots[data.selected_slot_index]), data.recipient_message);
}

async function acceptInvite() {
    if (previewMode) return;
    const otherInput = byID('other-freeform');
    const otherSelected = recipientSelectedIdeas.has('other');
    const customIdea = otherSelected ? otherInput.value.trim() : '';
    if (otherSelected && !customIdea) {
        otherInput.setAttribute('aria-invalid', 'true');
        showError('accept-error', 'Tell us what you would prefer for “Other”.');
        otherInput.focus();
        updateAcceptButton();
        return;
    }
    const selectedTime = document.querySelector('input[name="time-radio"]:checked');
    if (!selectedTime || byID('yes-btn').disabled) return;
    const selectedIDs = [...recipientSelectedIdeas].filter((id) => id !== 'other');
    const recipientMessage = byID('recipient-message').value.trim();
    const slotIndex = Number(selectedTime.value);
    const labels = selectedIDs.map((id) => ideaLabel(id, currentInvite.custom_ideas));
    const emojis = selectedIDs.map((id) => ideaEmoji(id, currentInvite.custom_ideas));
    const button = byID('yes-btn');
    button.disabled = true;
    showError('accept-error', '');
    try {
        const result = await postJSON('/api/invites/accept', {
            token: currentInviteToken,
            selected_ideas: selectedIDs,
            custom_idea: customIdea,
            selected_slot_index: slotIndex,
            recipient_message: recipientMessage
        });
        if (!result || result.status !== 'accepted' || typeof result.expires_at !== 'string') throw new APIError(0, 'The server returned an invalid response.');
        renderAccepted(labels, emojis, customIdea, formatSlot(currentInvite.proposed_slots[slotIndex]), recipientMessage);
    } catch (error) {
        showError('accept-error', error.status === 409 ? 'This invitation has already been accepted.' : (error.message || 'Could not save your answer.'));
        updateAcceptButton();
    }
}

function ideaLabel(id, customIdeas) {
    const builtIn = ideaByID.get(id);
    if (builtIn) return builtIn.label;
    const custom = customIdeas.find((item) => item.id === id);
    return custom ? custom.title : id;
}

function ideaEmoji(id, customIdeas) {
    const builtIn = ideaByID.get(id);
    if (builtIn) return builtIn.emoji;
    const custom = customIdeas.find((item) => item.id === id);
    return custom ? custom.emoji : '✨';
}

function clearStatusRefreshSchedule(message = '') {
    window.clearTimeout(statusRefreshTimer);
    window.clearInterval(statusCountdownTimer);
    statusRefreshTimer = 0;
    statusCountdownTimer = 0;
    nextStatusRefreshAt = 0;
    const nextCheck = byID('status-next-check');
    nextCheck.textContent = message;
    nextCheck.classList.toggle('hidden', !message);
}

function updateStatusCountdown() {
    const seconds = Math.max(0, Math.ceil((nextStatusRefreshAt - Date.now()) / 1000));
    const nextCheck = byID('status-next-check');
    nextCheck.textContent = `Checking again in ${seconds}s`;
    nextCheck.classList.remove('hidden');
}

function scheduleStatusRefresh() {
    clearStatusRefreshSchedule();
    if (!statusAutoRefreshEnabled || !currentStatusToken || document.hidden || !navigator.onLine) return;
    nextStatusRefreshAt = Date.now() + statusRefreshInterval;
    updateStatusCountdown();
    statusCountdownTimer = window.setInterval(updateStatusCountdown, 1000);
    statusRefreshTimer = window.setTimeout(refreshStatus, statusRefreshInterval);
}

function renderStatus(data) {
    const details = byID('status-details');
    const inviteShare = byID('status-invite-share');
    const inviteShareDetails = byID('status-invite-share-details');
    const revisitInviteButton = byID('status-revisit-invite-btn');
    const summary = byID('status-summary');
    const acceptedRow = byID('status-accepted-row');
    const updatedRow = byID('status-updated-row');
    statusAutoRefreshEnabled = data.status === 'pending';
    byID('status-card').classList.remove('status-error-state');
    byID('status-title').textContent = `${data.recipient_name}'s Invite`;
    summary.classList.remove('hidden');
    byID('status-expires-row').classList.remove('hidden');
    byID('status-actions').classList.remove('hidden');
    currentStatusInviteURL = data.invite_url || '';
    if (currentStatusInviteURL) {
        byID('status-invite-label').textContent = `Share this to ${data.recipient_name}`;
        byID('status-invite-url').textContent = currentStatusInviteURL;
        inviteShareDetails.classList.toggle('hidden', data.status !== 'pending');
        revisitInviteButton.classList.toggle('hidden', data.status !== 'accepted');
        inviteShare.classList.remove('hidden');
    } else {
        byID('status-invite-url').textContent = '';
        inviteShareDetails.classList.add('hidden');
        revisitInviteButton.classList.add('hidden');
        inviteShare.classList.add('hidden');
    }
    if (data.status === 'pending') {
        byID('status-emoji').textContent = '💌';
        summary.textContent = 'Still waiting for a response.';
        summary.classList.add('status-summary-pending');
        details.classList.add('hidden');
        byID('status-response-ideas-icon').textContent = '🚀';
        byID('status-response-ideas').replaceChildren();
        byID('status-response-slot').textContent = '';
        byID('status-response-message').classList.add('hidden');
        byID('status-response-message-label').textContent = 'Message';
        byID('status-response-message-text').textContent = '';
        acceptedRow.classList.add('hidden');
        byID('status-accepted').textContent = '';
        byID('status-accepted').removeAttribute('datetime');
    } else {
        byID('status-emoji').textContent = '🎉✨';
        summary.textContent = "It's a date! Here's the accepted plan:";
        summary.classList.remove('status-summary-pending');
        const labels = data.selected_ideas.map((id) => ideaLabel(id, data.custom_ideas));
        const emojis = data.selected_ideas.map((id) => ideaEmoji(id, data.custom_ideas));
        if (data.custom_idea) {
            labels.push(data.custom_idea);
            emojis.push('🤔');
        }
        renderPlanIdeas(byID('status-response-ideas-icon'), byID('status-response-ideas'), labels, emojis);
        byID('status-response-slot').textContent = formatSlot(data.proposed_slots[data.selected_slot_index]);
        byID('status-response-message-label').textContent = `Message from ${data.recipient_name}`;
        byID('status-response-message-text').textContent = data.recipient_message;
        byID('status-response-message').classList.toggle('hidden', !data.recipient_message);
        details.classList.remove('hidden');
        byID('status-accepted').textContent = formatStatusDate(data.accepted_at);
        byID('status-accepted').dateTime = data.accepted_at;
        acceptedRow.classList.remove('hidden');
    }
    byID('status-expires').textContent = formatStatusDate(data.expires_at);
    byID('status-expires').dateTime = data.expires_at;
    updatedRow.classList.toggle('hidden', !statusAutoRefreshEnabled);
    if (statusAutoRefreshEnabled) {
        const checkedAt = new Date();
        byID('status-updated').textContent = formatStatusDate(checkedAt);
        byID('status-updated').dateTime = checkedAt.toISOString();
    } else {
        byID('status-updated').textContent = '';
        byID('status-updated').removeAttribute('datetime');
    }
    byID('status-metadata').classList.remove('hidden');
    showError('status-error', '');
}

function renderStatusError(message) {
    const checkedAt = new Date();
    currentStatusInviteURL = '';
    byID('status-card').classList.add('status-error-state');
    byID('status-summary').classList.add('hidden');
    byID('status-details').classList.add('hidden');
    byID('status-invite-share').classList.add('hidden');
    byID('status-accepted-row').classList.add('hidden');
    byID('status-expires-row').classList.add('hidden');
    byID('status-actions').classList.add('hidden');
    byID('status-updated').textContent = formatStatusDate(checkedAt);
    byID('status-updated').dateTime = checkedAt.toISOString();
    byID('status-updated-row').classList.remove('hidden');
    byID('status-metadata').classList.remove('hidden');
    showError('status-error', message);
}

async function refreshStatus() {
    if (!currentStatusToken || document.hidden) {
        clearStatusRefreshSchedule();
        return;
    }
    if (!navigator.onLine) {
        renderStatusError('You appear to be offline.');
        clearStatusRefreshSchedule('Checking resumes when online');
        return;
    }
    clearStatusRefreshSchedule();
    try {
        const data = await postJSON('/api/status/view', { token: currentStatusToken });
        if (!isStatusResponse(data)) throw new APIError(0, 'The server returned an invalid response.');
        renderStatus(data);
    } catch (error) {
        if (error.status === 404) {
            statusAutoRefreshEnabled = false;
            const unavailableCard = byID('unavailable-card');
            unavailableCard.classList.add('status-unavailable-state');
            unavailableCard.querySelector('h2').textContent = 'Invite Unavailable or Expired';
            unavailableCard.querySelector('p').classList.add('hidden');
            showScreen('unavailable-card');
            return;
        }
        renderStatusError(error.message || 'Could not refresh the status.');
    } finally {
        scheduleStatusRefresh();
    }
}

async function deleteInvite() {
    if (!window.confirm('Permanently delete this invitation? Both links will stop working, and this cannot be undone.')) return;
    const button = byID('status-delete-btn');
    button.disabled = true;
    try {
        await postJSON('/api/status/delete', { token: currentStatusToken });
        currentStatusToken = '';
        currentStatusInviteURL = '';
        statusAutoRefreshEnabled = false;
        clearStatusRefreshSchedule();
        byID('status-emoji').textContent = '🗑️';
        byID('status-title').textContent = 'Invite Deleted';
        byID('status-summary').textContent = 'Both invitation links have been permanently deleted.';
        byID('status-summary').classList.remove('status-summary-pending');
        byID('status-details').classList.add('hidden');
        byID('status-invite-share').classList.add('hidden');
        byID('status-metadata').classList.add('hidden');
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
        if (data.status === 'accepted') renderAcceptedResponse(data); else renderRecipientView(data);
    } catch (error) {
        byID('unavailable-card').classList.remove('status-unavailable-state');
        byID('unavailable-card').querySelector('h2').textContent = 'Invite Unavailable';
        byID('unavailable-card').querySelector('p').classList.remove('hidden');
        byID('unavailable-card').querySelector('p').textContent = error.status === 404
            ? 'This invite link does not exist, has expired, or was deleted.'
            : 'This invitation could not be loaded. Please try again.';
        showScreen('unavailable-card');
    }
}

function routeFromHash() {
    const inviteMatch = location.hash.match(/^#\/invite\/([A-Za-z0-9_-]{22})$/);
    const statusMatch = location.hash.match(/^#\/status\/([A-Za-z0-9_-]{22})$/);
    if (inviteMatch) {
        currentStatusToken = '';
        currentStatusInviteURL = '';
        statusAutoRefreshEnabled = false;
        clearStatusRefreshSchedule();
        loadInvite(inviteMatch[1]);
        return;
    }
    if (statusMatch) {
        currentInviteToken = '';
        currentStatusToken = statusMatch[1];
        statusAutoRefreshEnabled = true;
        showScreen('status-card');
        refreshStatus();
        return;
    }
    currentInviteToken = '';
    currentStatusToken = '';
    currentStatusInviteURL = '';
    statusAutoRefreshEnabled = false;
    clearStatusRefreshSchedule();
    showScreen('landing-page');
}

function setupNoButton() {
    const noButton = byID('no-btn');
    function dodge() {
        if (previewMode) return;
        if (noButton.parentElement !== document.body) setupInitialNoButtonPosition();
        const current = noButton.getBoundingClientRect();
        noButton.classList.add('dodge-ready');
        const padding = 30;
        const maxX = Math.max(padding, window.innerWidth - noButton.offsetWidth - padding);
        const maxY = Math.max(padding, window.innerHeight - noButton.offsetHeight - padding);
        const acceptRect = byID('yes-btn').getBoundingClientRect();
        const overlapGap = 12;
        const overlapsAccept = (target) => (
            target.x < acceptRect.right + overlapGap
            && target.x + noButton.offsetWidth > acceptRect.left - overlapGap
            && target.y < acceptRect.bottom + overlapGap
            && target.y + noButton.offsetHeight > acceptRect.top - overlapGap
        );
        const pathCrossesAccept = (target) => {
            const bounds = {
                left: acceptRect.left - noButton.offsetWidth - overlapGap,
                right: acceptRect.right + overlapGap,
                top: acceptRect.top - noButton.offsetHeight - overlapGap,
                bottom: acceptRect.bottom + overlapGap
            };
            let entry = 0;
            let exit = 1;
            for (const [start, delta, minimum, maximum] of [
                [current.left, target.x - current.left, bounds.left, bounds.right],
                [current.top, target.y - current.top, bounds.top, bounds.bottom]
            ]) {
                if (Math.abs(delta) < 0.001) {
                    if (start < minimum || start > maximum) return false;
                    continue;
                }
                const first = (minimum - start) / delta;
                const second = (maximum - start) / delta;
                entry = Math.max(entry, Math.min(first, second));
                exit = Math.min(exit, Math.max(first, second));
                if (entry > exit) return false;
            }
            return exit > 0.001 && entry < 0.999;
        };
        const distanceTo = (target) => Math.hypot(target.x - current.left, target.y - current.top);
        const safeTargets = [
            { x: padding, y: padding },
            { x: maxX, y: padding },
            { x: padding, y: maxY },
            { x: maxX, y: maxY }
        ].filter((target) => !overlapsAccept(target) && !pathCrossesAccept(target));
        if (safeTargets.length === 0) return;
        const farthestTarget = safeTargets.reduce((farthest, target) => (
            distanceTo(target) > distanceTo(farthest) ? target : farthest
        ));
        const minimumTravel = Math.min(120, distanceTo(farthestTarget));
        let target = farthestTarget;
        for (let attempt = 0; attempt < 40; attempt += 1) {
            const candidate = {
                x: Math.floor(Math.random() * (maxX - padding + 1) + padding),
                y: Math.floor(Math.random() * (maxY - padding + 1) + padding)
            };
            if (!overlapsAccept(candidate) && !pathCrossesAccept(candidate) && distanceTo(candidate) >= minimumTravel) {
                target = candidate;
                break;
            }
        }
        noButton.style.left = `${target.x + window.scrollX}px`;
        noButton.style.top = `${target.y + window.scrollY}px`;
    }
    noButton.addEventListener('mouseover', dodge);
    noButton.addEventListener('touchstart', (event) => { event.preventDefault(); dodge(); }, { passive: false });
}

function setupInitialNoButtonPosition() {
    const card = byID('recipient-card');
    const noButton = byID('no-btn');
    if (card.classList.contains('hidden')) return;
    prepareNoButtonPosition();
    const rect = noButton.getBoundingClientRect();
    document.body.appendChild(noButton);
    noButton.style.left = `${rect.left + window.scrollX}px`;
    noButton.style.top = `${rect.top + window.scrollY}px`;
    noButton.style.position = 'absolute';
}

function prepareNoButtonPosition() {
    const noButton = byID('no-btn');
    const wrapper = document.querySelector('.no-btn-wrapper');
    if (noButton.parentElement !== wrapper) wrapper.appendChild(noButton);
    noButton.classList.remove('dodge-ready');
    noButton.style.position = '';
    noButton.style.left = '';
    noButton.style.top = '';
}

document.addEventListener('DOMContentLoaded', () => {
    setupCreatorIdeas();
    createCustomPickerRow();
    setupNoButton();
    byID('start-btn').addEventListener('click', () => showScreen('asker-card'));
    byID('add-slot-trigger').addEventListener('click', createCustomPickerRow);
    byID('add-custom-idea-trigger').addEventListener('click', createCustomIdeaRow);
    byID('creator-preview-btn').addEventListener('click', previewDraft);
    byID('preview-generate-btn').addEventListener('click', createInvite);
    byID('generated-invite-box').addEventListener('click', () => copyLink('generated-invite-url', 'copy-invite-btn', 'Copy Invite Link 📋'));
    byID('copy-invite-btn').addEventListener('click', () => copyLink('generated-invite-url', 'copy-invite-btn', 'Copy Invite Link 📋'));
    byID('generated-status-box').addEventListener('click', () => copyLink('generated-status-url', 'copy-status-btn', 'Copy Private Status Link 🔒'));
    byID('copy-status-btn').addEventListener('click', () => copyLink('generated-status-url', 'copy-status-btn', 'Copy Private Status Link 🔒'));
    byID('status-invite-box').addEventListener('click', () => copyLink('status-invite-url', 'status-copy-invite-btn', 'Copy Invite Link 📋'));
    byID('status-copy-invite-btn').addEventListener('click', () => copyLink('status-invite-url', 'status-copy-invite-btn', 'Copy Invite Link 📋'));
    byID('status-revisit-invite-btn').addEventListener('click', () => {
        if (currentStatusInviteURL) window.open(currentStatusInviteURL, '_blank', 'noopener,noreferrer');
    });
    byID('preview-btn').addEventListener('click', () => renderRecipientView(activeInviteData, 'links'));
    byID('share-back-btn').addEventListener('click', startNewInvite);
    byID('preview-back-btn').addEventListener('click', () => showScreen(previewSource === 'draft' ? 'asker-card' : 'share-card'));
    byID('sender-message').addEventListener('input', () => {
        byID('sender-message-count').textContent = String([...byID('sender-message').value].length);
    });
    byID('recipient-message').addEventListener('input', () => {
        byID('recipient-message-count').textContent = String([...byID('recipient-message').value].length);
    });
    byID('other-freeform').addEventListener('input', () => {
        if (byID('other-freeform').value.trim()) byID('other-freeform').setAttribute('aria-invalid', 'false');
        updateAcceptButton();
    });
    byID('yes-btn').addEventListener('click', acceptInvite);
    byID('accepted-replay-confetti-btn').addEventListener('click', startCelebration);
    byID('status-delete-btn').addEventListener('click', deleteInvite);
    byID('unavailable-create-btn').addEventListener('click', startNewInvite);
    document.addEventListener('visibilitychange', () => {
        if (document.hidden) clearStatusRefreshSchedule(); else if (statusAutoRefreshEnabled) refreshStatus();
    });
    document.addEventListener('click', () => closeEmojiPalettes());
    window.addEventListener('online', () => { if (statusAutoRefreshEnabled) refreshStatus(); });
    window.addEventListener('offline', () => {
        if (currentStatusToken) renderStatusError('You appear to be offline.');
        clearStatusRefreshSchedule('Checking resumes when online');
    });
    window.addEventListener('hashchange', routeFromHash);
    routeFromHash();
});
