package mls

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"

	"github.com/bifurcation/mint/syntax"
)

///
/// GroupContext
///
type GroupContext struct {
	GroupID                 []byte `tls:"head=1"`
	Epoch                   Epoch
	TreeHash                []byte `tls:"head=1"`
	ConfirmedTranscriptHash []byte `tls:"head=1"`
	Extensions              ExtensionList
}

///
/// State
///

type ProposalRef uint64

func toRef(id ProposalID) ProposalRef {
	ref := uint64(0)
	for i := uint(0); i < 8; i++ {
		ref |= uint64(id.Hash[i]) << i
	}
	return ProposalRef(ref)
}

var supportedGroupExtensions = []ExtensionType{
	// TODO
}

type State struct {
	// Shared confirmed state
	CipherSuite             CipherSuite
	GroupID                 []byte `tls:"head=1"`
	Epoch                   Epoch
	Tree                    RatchetTree
	ConfirmedTranscriptHash []byte `tls:"head=1"`
	InterimTranscriptHash   []byte `tls:"head=1"`
	Extensions              ExtensionList

	// Per-participant non-secret state
	Index            leafIndex           `tls:"omit"`
	IdentityPriv     SignaturePrivateKey `tls:"omit"`
	Scheme           SignatureScheme     `tls:"omit"`
	PendingProposals []MLSPlaintext      `tls:"omit"`

	// Secret state
	UpdateKeys map[ProposalRef]HPKEPrivateKey `tls:"omit"`
	Keys       keyScheduleEpoch               `tls:"omit"`
}

func NewEmptyState(groupID []byte, kp KeyPackage) (*State, error) {
	return NewEmptyStateWithExtensions(groupID, kp, NewExtensionList())
}

func NewEmptyStateWithExtensions(groupID []byte, kp KeyPackage, ext ExtensionList) (*State, error) {
	suite := kp.CipherSuite
	tree := NewRatchetTree(suite)
	tree.AddLeaf(0, kp)

	// Verify that the creator supports the group's extensions
	for _, ext := range ext.Entries {
		if !kp.Extensions.Has(ext.ExtensionType) {
			return nil, fmt.Errorf("Unsupported extension type [%04x]", ext.ExtensionType)
		}
	}

	secret := make([]byte, suite.newDigest().Size())
	kse := newKeyScheduleEpoch(suite, 1, secret, []byte{})
	s := &State{
		CipherSuite:             kp.CipherSuite,
		GroupID:                 groupID,
		Epoch:                   0,
		Tree:                    *tree,
		Keys:                    kse,
		Index:                   0,
		IdentityPriv:            *kp.Credential.privateKey,
		Scheme:                  kp.Credential.Scheme(),
		UpdateKeys:              map[ProposalRef]HPKEPrivateKey{},
		ConfirmedTranscriptHash: []byte{},
		InterimTranscriptHash:   []byte{},
		Extensions:              ext,
	}
	return s, nil
}

func NewStateFromWelcome(suite CipherSuite, epochSecret []byte, welcome Welcome) (*State, leafIndex, []byte, error) {
	// Decrypt the GroupInfo
	gi, err := welcome.Decrypt(suite, epochSecret)
	if err != nil {
		return nil, 0, nil, err
	}

	// Construct the new state
	s := &State{
		CipherSuite:             suite,
		Epoch:                   gi.Epoch,
		Tree:                    gi.Tree.Clone(),
		GroupID:                 gi.GroupID,
		ConfirmedTranscriptHash: gi.ConfirmedTranscriptHash,
		InterimTranscriptHash:   gi.InterimTranscriptHash,
		Extensions:              gi.Extensions,
		PendingProposals:        []MLSPlaintext{},
		UpdateKeys:              map[ProposalRef]HPKEPrivateKey{},
	}

	return s, gi.SignerIndex, gi.Confirmation, nil
}

func NewJoinedState(kps []KeyPackage, welcome Welcome) (*State, error) {
	var keyPackage KeyPackage
	var encGroupSecrets EncryptedGroupSecrets
	var found = false
	suite := welcome.CipherSuite
	// extract the keyPackage for init secret
	for idx, kp := range kps {
		data, err := syntax.Marshal(kp)
		if err != nil {
			return nil, fmt.Errorf("mls.state: kp %d marshal failure %v", idx, err)
		}
		kphash := welcome.CipherSuite.digest(data)
		// parse the encryptedKeyPackage to find our right kp
		for _, egs := range welcome.Secrets {
			found = bytes.Equal(kphash, egs.KeyPackageHash)
			if found {
				keyPackage = kp
				encGroupSecrets = egs
				break
			}
		}
		if found {
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("mls.state: unable to decrypt welcome message")
	}

	if keyPackage.CipherSuite != welcome.CipherSuite {
		return nil, fmt.Errorf("mls.state: ciphersuite mismatch")
	}

	if keyPackage.privateKey == nil {
		return nil, fmt.Errorf("mls.state: no private key for init key")
	}

	if keyPackage.Credential.privateKey == nil {
		return nil, fmt.Errorf("mls.state: no signing key for init key")
	}

	pt, err := suite.hpke().Decrypt(*keyPackage.privateKey, []byte{}, encGroupSecrets.EncryptedGroupSecrets)
	if err != nil {
		return nil, fmt.Errorf("mls.state: encKeyPkg decryption failure %v", err)
	}

	var groupSecrets GroupSecrets
	_, err = syntax.Unmarshal(pt, &groupSecrets)
	if err != nil {
		return nil, fmt.Errorf("mls.state: keyPkg unmarshal failure %v", err)
	}

	// Construct a new state based on the GroupInfo
	s, signerIndex, confirmation, err := NewStateFromWelcome(suite, groupSecrets.EpochSecret, welcome)
	if err != nil {
		return nil, err
	}

	s.IdentityPriv = *keyPackage.Credential.privateKey
	s.Scheme = keyPackage.Credential.Scheme()

	// Verify that the joiner supports the group's extensions
	for _, ext := range s.Extensions.Entries {
		if !keyPackage.Extensions.Has(ext.ExtensionType) {
			return nil, fmt.Errorf("Unsupported extension type [%04x]", ext.ExtensionType)
		}
	}

	// add self to tree
	index, res := s.Tree.Find(keyPackage)
	if !res {
		return nil, fmt.Errorf("mls.state: new joiner not in the tree")
	}
	s.Index = index
	err = s.Tree.SetLeafPrivateKeys(s.Index, *keyPackage.privateKey, s.IdentityPriv)
	if err != nil {
		return nil, err
	}

	// implant the provided path secrets in the tree
	commonAncestor := ancestor(s.Index, signerIndex)
	_, err = s.Tree.Implant(commonAncestor, groupSecrets.PathSecret)

	encGrpCtx, err := syntax.Marshal(s.groupContext())
	if err != nil {
		return nil, fmt.Errorf("mls.state: groupCtx marshal failure %v", err)
	}

	s.Keys = newKeyScheduleEpoch(suite, leafCount(s.Tree.size()), groupSecrets.EpochSecret, encGrpCtx)

	// confirmation verification
	hmac := suite.newHMAC(s.Keys.ConfirmationKey)
	hmac.Write(s.ConfirmedTranscriptHash)
	localConfirmation := hmac.Sum(nil)
	if !bytes.Equal(localConfirmation, confirmation) {
		return nil, fmt.Errorf("mls.state: confirmation failed to verify")
	}

	return s, nil
}

func negotiateWithPeer(groupID []byte, myKPs, otherKPs []KeyPackage, commitSecret []byte) (*Welcome, *State, error) {
	var selected = false
	var mySelectedKP, otherSelectedKP KeyPackage

	for _, mykp := range myKPs {
		for _, okp := range otherKPs {
			if mykp.CipherSuite == okp.CipherSuite && mykp.Version == okp.Version {
				selected = true
				mySelectedKP = mykp
				otherSelectedKP = okp
				break
			}
		}
		if selected {
			break
		}
	}

	if !selected {
		return nil, nil, fmt.Errorf("mls.state: negotiation failure")
	}

	// init our state and add the negotiated peer's kp
	s, err := NewEmptyState(groupID, mySelectedKP)
	if err != nil {
		return nil, nil, err
	}

	add, err := s.Add(otherSelectedKP)
	if err != nil {
		return nil, nil, err
	}

	// update tree state
	_, err = s.Handle(add)
	if err != nil {
		return nil, nil, err
	}

	// commit the add and generate welcome to be sent to the peer
	_, welcome, newState, err := s.Commit(commitSecret)
	if err != nil {
		return nil, nil, err
	}

	return welcome, newState, nil
}

func (s State) KeyPackage() KeyPackage {
	kp, ok := s.Tree.KeyPackage(s.Index)
	if !ok {
		panic("Malformed state")
	}

	return kp
}

func (s State) Add(kp KeyPackage) (*MLSPlaintext, error) {
	// Verify that the new member supports the group's extensions
	for _, ext := range s.Extensions.Entries {
		if !kp.Extensions.Has(ext.ExtensionType) {
			return nil, fmt.Errorf("Unsupported extension type [%04x]", ext.ExtensionType)
		}
	}

	addProposal := Proposal{
		Add: &AddProposal{
			KeyPackage: kp,
		},
	}
	return s.sign(addProposal), nil
}

func (s State) Update(kp KeyPackage) *MLSPlaintext {
	updateProposal := Proposal{
		Update: &UpdateProposal{
			KeyPackage: kp,
		},
	}

	pt := s.sign(updateProposal)
	ref := toRef(s.proposalID(*pt))
	s.UpdateKeys[ref] = *kp.privateKey
	return pt
}

func (s *State) Remove(removed leafIndex) *MLSPlaintext {
	removeProposal := Proposal{
		Remove: &RemoveProposal{
			Removed: removed,
		},
	}
	return s.sign(removeProposal)
}

func (s *State) Commit(leafSecret []byte) (*MLSPlaintext, *Welcome, *State, error) {
	// Construct and apply a commit message
	commit := Commit{}
	var joiners []KeyPackage

	for _, pp := range s.PendingProposals {
		pid := s.proposalID(pp)
		proposal := pp.Content.Proposal
		switch proposal.Type() {
		case ProposalTypeAdd:
			commit.Adds = append(commit.Adds, pid)
			joiners = append(joiners, proposal.Add.KeyPackage)
		case ProposalTypeUpdate:
			commit.Updates = append(commit.Updates, pid)
		case ProposalTypeRemove:
			commit.Removes = append(commit.Removes, pid)
		}
	}

	// init new state to apply commit and ratchet forward
	next := s.Clone()
	err := next.apply(commit)
	if err != nil {
		return nil, nil, nil, err
	}

	// reset after commit the proposals
	next.PendingProposals = nil

	// KEM new entropy to the new group
	ctx, err := syntax.Marshal(next.groupContext())
	if err != nil {
		return nil, nil, nil, err
	}

	var leafParentHash, commitSecret []byte
	commit.Path, leafParentHash, commitSecret, err = next.Tree.Encap(s.Index, ctx, leafSecret)
	if err != nil {
		return nil, nil, nil, err
	}

	// Update the leaf for this member
	kp := s.KeyPackage()
	err = kp.UpdateInitKey()
	if err != nil {
		return nil, nil, nil, err
	}

	phe := ParentHashExtension{leafParentHash}
	err = kp.SetExtensions([]ExtensionBody{phe})
	if err != nil {
		return nil, nil, nil, err
	}

	err = kp.Sign()
	if err != nil {
		return nil, nil, nil, err
	}

	commit.KeyPackage = kp
	err = next.Tree.SetLeaf(s.Index, kp)
	if err != nil {
		return nil, nil, nil, err
	}

	// Create the Commit message and advance the transcripts / key schedule
	pt, err := next.ratchetAndSign(commit, commitSecret, s.groupContext())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mls.state: racthet forward failed %v", err)
	}

	// Complete the GroupInfo and form the Welcome
	gi := &GroupInfo{
		GroupID:                 next.GroupID,
		Epoch:                   next.Epoch,
		Tree:                    next.Tree,
		ConfirmedTranscriptHash: next.ConfirmedTranscriptHash,
		InterimTranscriptHash:   next.InterimTranscriptHash,
		Confirmation:            pt.Content.Commit.Confirmation.Data,
	}
	err = gi.sign(s.Index, &s.IdentityPriv)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mls.state: groupInfo sign failure %v", err)
	}

	welcome := newWelcome(s.CipherSuite, next.Keys.EpochSecret, gi)
	pathSecrets := next.Tree.PathSecrets(toNodeIndex(next.Index), leafSecret)
	for _, kp := range joiners {
		leaf, ok := next.Tree.Find(kp)
		if !ok {
			return nil, nil, nil, fmt.Errorf("mls.state: New joiner not in tree")
		}

		commonAncestor := ancestor(leaf, next.Index)
		pathSecret, ok := pathSecrets[commonAncestor]
		if !ok {
			return nil, nil, nil, fmt.Errorf("mls.state: No path secret for new joiner")
		}

		welcome.EncryptTo(kp, pathSecret)
	}

	return pt, welcome, next, nil
}

/// Proposal processing helpers

func (s *State) apply(commit Commit) error {
	// state to identify proposals being processed
	// in the PendingProposals. Avoids linear loop to
	// remove entries from PendingProposals.
	var processedProposals = map[string]bool{}
	err := s.applyProposals(commit.Updates, processedProposals)
	if err != nil {
		return err
	}

	err = s.applyProposals(commit.Removes, processedProposals)
	if err != nil {
		return err
	}

	err = s.applyProposals(commit.Adds, processedProposals)
	if err != nil {
		return err
	}

	return nil
}

func (s *State) applyAddProposal(add *AddProposal) error {
	if add.KeyPackage.CipherSuite != s.CipherSuite {
		return fmt.Errorf("mls.state: new member kp does not use group ciphersuite")
	}

	if !add.KeyPackage.Verify() {
		return fmt.Errorf("mls.state: Invalid kp")
	}

	target := s.Tree.LeftmostFree()
	return s.Tree.AddLeaf(target, add.KeyPackage)
}

func (s *State) applyRemoveProposal(remove *RemoveProposal) error {
	return s.Tree.BlankPath(leafIndex(remove.Removed))
}

func (s *State) applyUpdateProposal(target leafIndex, update *UpdateProposal) error {
	if update.KeyPackage.CipherSuite != s.CipherSuite {
		panic(fmt.Errorf("mls.state: update kp does not use group ciphersuite %v != %v", update.KeyPackage.CipherSuite, s.CipherSuite))
	}

	if !update.KeyPackage.Verify() {
		return fmt.Errorf("mls.state: Invalid kp")
	}

	return s.Tree.UpdateLeaf(target, update.KeyPackage)
}

func (s *State) applyProposals(ids []ProposalID, processed map[string]bool) error {
	for _, id := range ids {
		pt, ok := s.findProposal(id)
		if !ok {
			return fmt.Errorf("mls.state: commit of unknown proposal %s", id)
		}

		// we have processed this proposal already
		if processed[id.String()] {
			continue
		} else {
			processed[id.String()] = true
		}

		proposal := pt.Content.Proposal
		switch proposal.Type() {
		case ProposalTypeAdd:
			err := s.applyAddProposal(proposal.Add)
			if err != nil {
				return err
			}
		case ProposalTypeUpdate:
			if pt.Sender.Type != SenderTypeMember {
				return fmt.Errorf("mls.state: update from non-member")
			}

			senderIndex := leafIndex(pt.Sender.Sender)
			err := s.applyUpdateProposal(senderIndex, proposal.Update)
			if err != nil {
				return err
			}

			if senderIndex == s.Index {
				updatePriv, ok := s.UpdateKeys[toRef(id)]
				if !ok {
					return fmt.Errorf("mls.state: self-update with no cached secret")
				}

				err := s.Tree.SetLeafPrivateKeys(senderIndex, updatePriv, s.IdentityPriv)
				if err != nil {
					return err
				}
			}

		case ProposalTypeRemove:
			err := s.applyRemoveProposal(proposal.Remove)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("mls.state: invalid proposal type")
		}
	}
	return nil
}

func (s State) findProposal(id ProposalID) (MLSPlaintext, bool) {
	for _, pt := range s.PendingProposals {
		otherPid := s.proposalID(pt)
		if bytes.Equal(otherPid.Hash, id.Hash) {
			return pt, true
		}
	}
	// we can return may be reference
	// regardless, the call has to do a check before
	// using the returned value
	return MLSPlaintext{}, false
}

func (s State) proposalID(plaintext MLSPlaintext) ProposalID {
	enc, err := syntax.Marshal(plaintext)
	if err != nil {
		panic(fmt.Errorf("mls.state: mlsPlainText marshal failure %v", err))

	}
	return ProposalID{
		Hash: s.CipherSuite.digest(enc),
	}
}

func (s State) groupContext() GroupContext {
	return GroupContext{
		GroupID:                 s.GroupID,
		Epoch:                   s.Epoch,
		TreeHash:                s.Tree.RootHash(),
		ConfirmedTranscriptHash: s.ConfirmedTranscriptHash,
		Extensions:              s.Extensions,
	}
}

func (s State) sign(p Proposal) *MLSPlaintext {
	pt := &MLSPlaintext{
		GroupID: s.GroupID,
		Epoch:   s.Epoch,
		Sender:  Sender{SenderTypeMember, uint32(s.Index)},
		Content: MLSPlaintextContent{
			Proposal: &p,
		},
	}

	pt.sign(s.groupContext(), s.IdentityPriv, s.Scheme)
	return pt
}

func (s *State) updateEpochSecrets(secret []byte) {
	ctx, err := syntax.Marshal(GroupContext{
		GroupID:                 s.GroupID,
		Epoch:                   s.Epoch,
		TreeHash:                s.Tree.RootHash(),
		ConfirmedTranscriptHash: s.ConfirmedTranscriptHash,
	})
	if err != nil {
		panic(fmt.Errorf("mls.state: update epoch secret failed %v", err))
	}

	// TODO(RLB) Provide an API to provide PSKs
	s.Keys = s.Keys.Next(leafCount(s.Tree.size()), nil, secret, ctx)
}

func (s *State) ratchetAndSign(op Commit, commitSecret []byte, prevGrpCtx GroupContext) (*MLSPlaintext, error) {
	pt := &MLSPlaintext{
		GroupID: s.GroupID,
		Epoch:   s.Epoch,
		Sender:  Sender{SenderTypeMember, uint32(s.Index)},
		Content: MLSPlaintextContent{
			Commit: &CommitData{
				Commit: op,
			},
		},
	}

	// Update the Confirmed Transcript Hash
	digest := s.CipherSuite.newDigest()
	digest.Write(s.InterimTranscriptHash)
	digest.Write(pt.commitContent())
	s.ConfirmedTranscriptHash = digest.Sum(nil)

	// Advance the key schedule
	s.Epoch += 1
	s.updateEpochSecrets(commitSecret)

	// generate the confirmation based on the new keys
	commit := pt.Content.Commit
	hmac := s.CipherSuite.newHMAC(s.Keys.ConfirmationKey)
	hmac.Write(s.ConfirmedTranscriptHash)
	commit.Confirmation.Data = hmac.Sum(nil)

	// sign the MLSPlainText and update state hashes
	// as a result of ratcheting.
	pt.sign(prevGrpCtx, s.IdentityPriv, s.Scheme)

	authData, err := pt.commitAuthData()
	if err != nil {
		return nil, err
	}

	digest = s.CipherSuite.newDigest()
	digest.Write(s.ConfirmedTranscriptHash)
	digest.Write(authData)
	s.InterimTranscriptHash = digest.Sum(nil)

	return pt, nil
}

func (s State) signerPublicKey(sender Sender) (*SignaturePublicKey, error) {
	switch sender.Type {
	case SenderTypeMember:
		kp, ok := s.Tree.KeyPackage(leafIndex(sender.Sender))
		if !ok {
			return nil, fmt.Errorf("mls.state: Received from blank leaf")
		}

		return kp.Credential.PublicKey(), nil

	default:
		// TODO(RLB): Support add sent by new member
		// TODO(RLB): Support add/remove signed by preconfigured key
		return nil, fmt.Errorf("mls.state: Unsupported sender type")
	}
}

func (s *State) Handle(pt *MLSPlaintext) (*State, error) {
	if !bytes.Equal(pt.GroupID, s.GroupID) {
		return nil, fmt.Errorf("mls.state: groupId mismatch")
	}

	if pt.Epoch != s.Epoch {
		return nil, fmt.Errorf("mls.state: epoch mismatch, have %v, got %v", s.Epoch, pt.Epoch)
	}

	sigPubKey, err := s.signerPublicKey(pt.Sender)
	if err != nil {
		return nil, err
	}

	if !pt.verify(s.groupContext(), sigPubKey, s.Scheme) {
		return nil, fmt.Errorf("invalid handshake message signature")
	}

	// Proposals get queued, do not result in a state transition
	contentType := pt.Content.Type()
	if contentType == ContentTypeProposal {
		s.PendingProposals = append(s.PendingProposals, *pt)
		return nil, nil
	}

	if contentType != ContentTypeCommit {
		return nil, fmt.Errorf("mls.state: incorrect content type")
	} else if pt.Sender.Type != SenderTypeMember {
		return nil, fmt.Errorf("mls.state: commit from non-member")
	}

	if leafIndex(pt.Sender.Sender) == s.Index {
		return nil, fmt.Errorf("mls.state: handle own commits with caching")
	}

	// apply the commit and discard any remaining pending proposals
	senderIndex := leafIndex(pt.Sender.Sender)
	commitData := pt.Content.Commit
	next := s.Clone()
	err = next.apply(commitData.Commit)
	if err != nil {
		return nil, err
	}

	next.PendingProposals = next.PendingProposals[:0]

	// apply the direct path
	ctx, err := syntax.Marshal(GroupContext{
		GroupID:                 next.GroupID,
		Epoch:                   next.Epoch,
		TreeHash:                next.Tree.RootHash(),
		ConfirmedTranscriptHash: next.ConfirmedTranscriptHash,
	})
	if err != nil {
		return nil, fmt.Errorf("mls.state: failure to create context %v", err)
	}

	commitSecret, err := next.Tree.Decap(senderIndex, ctx, commitData.Commit.Path)
	if err != nil {
		return nil, err
	}

	// Apply the update in the commit
	err = next.Tree.SetLeaf(senderIndex, commitData.Commit.KeyPackage)
	if err != nil {
		return nil, err
	}

	// Update the confirmed transcript hash
	digest := next.CipherSuite.newDigest()
	digest.Write(next.InterimTranscriptHash)
	digest.Write(pt.commitContent())
	next.ConfirmedTranscriptHash = digest.Sum(nil)

	// Advance the key schedule
	next.Epoch += 1
	next.updateEpochSecrets(commitSecret)

	// Verify confirmation MAC
	if !next.verifyConfirmation(commitData.Confirmation.Data) {
		return nil, fmt.Errorf("mls.state: confirmation failed to verify")
	}

	authData, err := pt.commitAuthData()
	if err != nil {
		return nil, err
	}

	// Update the interim transcript hash
	digest = next.CipherSuite.newDigest()
	digest.Write(next.ConfirmedTranscriptHash)
	digest.Write(authData)
	next.InterimTranscriptHash = digest.Sum(nil)

	return next, nil
}

///// protect/unprotect and helpers

func (s State) verifyConfirmation(confirmation []byte) bool {
	// confirmation verification
	hmac := s.CipherSuite.newHMAC(s.Keys.ConfirmationKey)
	hmac.Write(s.ConfirmedTranscriptHash)
	confirm := hmac.Sum(nil)
	if !bytes.Equal(confirm, confirmation) {
		return false
	}
	return true
}

func applyGuard(nonceIn []byte, reuseGuard [4]byte) []byte {
	nonceOut := dup(nonceIn)
	for i := range reuseGuard {
		nonceOut[i] ^= reuseGuard[i]
	}
	return nonceOut
}

func (s *State) encrypt(pt *MLSPlaintext) (*MLSCiphertext, error) {
	var generation uint32
	var keys keyAndNonce
	switch pt.Content.Type() {
	case ContentTypeApplication:
		generation, keys = s.Keys.ApplicationKeys.Next(s.Index)
	case ContentTypeProposal, ContentTypeCommit:
		generation, keys = s.Keys.HandshakeKeys.Next(s.Index)
	default:
		return nil, fmt.Errorf("mls.state: encrypt unknown content type")
	}

	var reuseGuard [4]byte
	rand.Read(reuseGuard[:])

	stream := NewWriteStream()
	err := stream.WriteAll(s.Index, generation, reuseGuard)
	if err != nil {
		return nil, fmt.Errorf("mls.state: sender data marshal failure %v", err)
	}

	senderData := stream.Data()
	senderDataNonce := make([]byte, s.CipherSuite.constants().NonceSize)
	rand.Read(senderDataNonce)
	senderDataAADVal := senderDataAAD(s.GroupID, s.Epoch, pt.Content.Type(), senderDataNonce)
	sdAead, _ := s.CipherSuite.newAEAD(s.Keys.SenderDataKey)
	sdCt := sdAead.Seal(nil, senderDataNonce, senderData, senderDataAADVal)

	// content data
	stream = NewWriteStream()
	err = stream.Write(pt.Content)
	if err == nil {
		err = stream.Write(pt.Signature)
	}
	if err != nil {
		return nil, fmt.Errorf("mls.state: content marshal failure %v", err)
	}
	content := stream.Data()

	aad := contentAAD(s.GroupID, s.Epoch, pt.Content.Type(),
		pt.AuthenticatedData, senderDataNonce, sdCt)
	aead, _ := s.CipherSuite.newAEAD(keys.Key)
	contentCt := aead.Seal(nil, applyGuard(keys.Nonce, reuseGuard), content, aad)

	// set up MLSCipherText
	ct := &MLSCiphertext{
		GroupID:             s.GroupID,
		Epoch:               s.Epoch,
		ContentType:         pt.Content.Type(),
		AuthenticatedData:   pt.AuthenticatedData,
		SenderDataNonce:     senderDataNonce,
		EncryptedSenderData: sdCt,
		Ciphertext:          contentCt,
	}

	return ct, nil
}

func (s *State) decrypt(ct *MLSCiphertext) (*MLSPlaintext, error) {
	if !bytes.Equal(ct.GroupID, s.GroupID) {
		return nil, fmt.Errorf("mls.state: ciphertext not from this group")
	}

	if ct.Epoch != s.Epoch {
		return nil, fmt.Errorf("mls.state: ciphertext not from this epoch")
	}

	// handle sender data
	sdAAD := senderDataAAD(ct.GroupID, ct.Epoch, ContentType(ct.ContentType), ct.SenderDataNonce)
	sdAead, _ := s.CipherSuite.newAEAD(s.Keys.SenderDataKey)
	sd, err := sdAead.Open(nil, ct.SenderDataNonce, ct.EncryptedSenderData, sdAAD)
	if err != nil {
		return nil, fmt.Errorf("mls.state: senderData decryption failure %v", err)
	}

	// parse the senderData
	var sender leafIndex
	var generation uint32
	var reuseGuard [4]byte
	stream := NewReadStream(sd)
	_, err = stream.ReadAll(&sender, &generation, &reuseGuard)
	if err != nil {
		return nil, fmt.Errorf("mls.state: senderData unmarshal failure %v", err)
	}

	var keys keyAndNonce
	contentType := ContentType(ct.ContentType)
	switch contentType {
	case ContentTypeApplication:
		keys, err = s.Keys.ApplicationKeys.Get(sender, generation)
		if err != nil {
			return nil, fmt.Errorf("mls.state: application keys extraction failed %v", err)
		}
		s.Keys.ApplicationKeys.Erase(sender, generation)
	case ContentTypeProposal, ContentTypeCommit:
		keys, err = s.Keys.HandshakeKeys.Get(sender, generation)
		if err != nil {
			return nil, fmt.Errorf("mls.state: handshake keys extraction failed %v", err)
		}
		s.Keys.HandshakeKeys.Erase(sender, generation)
	default:
		return nil, fmt.Errorf("mls.state: unsupported content type")
	}

	aad := contentAAD(ct.GroupID, ct.Epoch, ContentType(ct.ContentType),
		ct.AuthenticatedData, ct.SenderDataNonce, ct.EncryptedSenderData)
	aead, _ := s.CipherSuite.newAEAD(keys.Key)
	content, err := aead.Open(nil, applyGuard(keys.Nonce, reuseGuard), ct.Ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("mls.state: content decryption failure %v", err)
	}

	// parse the Content and Signature
	stream = NewReadStream(content)
	var mlsContent MLSPlaintextContent
	var signature Signature
	_, err = stream.Read(&mlsContent)
	if err == nil {
		_, err = stream.Read(&signature)
	}
	if err != nil {
		return nil, fmt.Errorf("mls.state: content unmarshal failure %v", err)
	}
	_, _ = syntax.Unmarshal(content, &mlsContent)

	pt := &MLSPlaintext{
		GroupID:           s.GroupID,
		Epoch:             s.Epoch,
		Sender:            Sender{SenderTypeMember, uint32(sender)},
		AuthenticatedData: ct.AuthenticatedData,
		Content:           mlsContent,
		Signature:         signature,
	}
	return pt, nil
}

func (s *State) Protect(data []byte) (*MLSCiphertext, error) {
	pt := &MLSPlaintext{
		GroupID: s.GroupID,
		Epoch:   s.Epoch,
		Sender:  Sender{SenderTypeMember, uint32(s.Index)},
		Content: MLSPlaintextContent{
			Application: &ApplicationData{
				Data: data,
			},
		},
	}

	pt.sign(s.groupContext(), s.IdentityPriv, s.Scheme)
	return s.encrypt(pt)
}

func (s *State) Unprotect(ct *MLSCiphertext) ([]byte, error) {
	pt, err := s.decrypt(ct)
	if err != nil {
		return nil, err
	}

	sigPubKey, err := s.signerPublicKey(pt.Sender)
	if err != nil {
		return nil, err
	}

	if !pt.verify(s.groupContext(), sigPubKey, s.Scheme) {
		return nil, fmt.Errorf("invalid message signature")
	}

	if pt.Content.Type() != ContentTypeApplication {
		return nil, fmt.Errorf("unprotect attempted on non-application message")
	}
	return pt.Content.Application.Data, nil
}

func senderDataAAD(gid []byte, epoch Epoch, contentType ContentType, nonce []byte) []byte {
	s := NewWriteStream()
	err := s.Write(struct {
		GroupID         []byte `tls:"head=1"`
		Epoch           Epoch
		ContentType     ContentType
		SenderDataNonce []byte `tls:"head=1"`
	}{
		GroupID:         gid,
		Epoch:           epoch,
		ContentType:     contentType,
		SenderDataNonce: nonce,
	})

	if err != nil {
		return nil
	}

	return s.Data()
}

func contentAAD(gid []byte, epoch Epoch,
	contentType ContentType, authenticatedData []byte,
	nonce []byte, encSenderData []byte) []byte {

	s := NewWriteStream()
	err := s.Write(struct {
		GroupID             []byte `tls:"head=1"`
		Epoch               Epoch
		ContentType         ContentType
		AuthenticatedData   []byte `tls:"head=4"`
		SenderDataNonce     []byte `tls:"head=1"`
		EncryptedSenderData []byte `tls:"head=1"`
	}{
		GroupID:             gid,
		Epoch:               epoch,
		ContentType:         contentType,
		AuthenticatedData:   authenticatedData,
		SenderDataNonce:     nonce,
		EncryptedSenderData: encSenderData,
	})

	if err != nil {
		return nil
	}
	return s.Data()
}

func (s State) Clone() *State {
	// Note: all the slice/map copy operations below on state are mere
	// reference copies.
	clone := &State{
		CipherSuite:             s.CipherSuite,
		GroupID:                 dup(s.GroupID),
		Epoch:                   s.Epoch,
		Tree:                    s.Tree.Clone(),
		ConfirmedTranscriptHash: nil,
		InterimTranscriptHash:   dup(s.InterimTranscriptHash),
		Keys:                    s.Keys,
		Index:                   s.Index,
		IdentityPriv:            s.IdentityPriv,
		Scheme:                  s.Scheme,
		UpdateKeys:              s.UpdateKeys,
		PendingProposals:        make([]MLSPlaintext, len(s.PendingProposals)),
	}

	copy(clone.PendingProposals, s.PendingProposals)
	return clone
}

// Compare the public and shared private aspects of two nodes
func (s State) Equals(o State) bool {
	suite := s.CipherSuite == o.CipherSuite
	groupID := bytes.Equal(s.GroupID, o.GroupID)
	epoch := s.Epoch == o.Epoch
	tree := s.Tree.Equals(o.Tree)
	cth := bytes.Equal(s.ConfirmedTranscriptHash, o.ConfirmedTranscriptHash)
	ith := bytes.Equal(s.InterimTranscriptHash, o.InterimTranscriptHash)
	keys := reflect.DeepEqual(s.Keys, o.Keys)

	return suite && groupID && epoch && tree && cth && ith && keys
}

// Isolated getters and setters for public and secret state
//
// Note that the get/set operations here are very shallow.  We basically assume
// that the StateSecrets object is temporary, as a carrier for marshaling /
// unmarshaling.
type StateSecrets struct {
	CipherSuite CipherSuite

	// Per-participant non-secret state
	Index            leafIndex
	InitPriv         HPKEPrivateKey
	IdentityPriv     SignaturePrivateKey
	Scheme           SignatureScheme
	PendingProposals []MLSPlaintext `tls:"head=4"`

	// Secret state
	UpdateKeys map[ProposalRef]HPKEPrivateKey `tls:"head=4"`
	Keys       keyScheduleEpoch
	Tree       TreeSecrets
}

func NewStateFromWelcomeAndSecrets(welcome Welcome, ss StateSecrets) (*State, error) {
	// Import the base data using some information from the secrets
	suite := ss.CipherSuite
	epochSecret := ss.Keys.EpochSecret
	s, _, confirmation, err := NewStateFromWelcome(suite, epochSecret, welcome)
	if err != nil {
		return nil, err
	}

	// Import the secrets
	s.SetSecrets(ss)

	// Verify the confirmation
	if !s.verifyConfirmation(confirmation) {
		return nil, fmt.Errorf("mls.state: Confirmation failed to verify")
	}

	return s, nil
}

func (s *State) SetSecrets(ss StateSecrets) {
	s.CipherSuite = ss.CipherSuite
	s.Index = ss.Index
	s.IdentityPriv = ss.IdentityPriv
	s.Scheme = ss.Scheme
	s.PendingProposals = ss.PendingProposals
	s.UpdateKeys = ss.UpdateKeys
	s.Keys = ss.Keys
	s.Tree.SetSecrets(ss.Tree)
	s.Tree.SetLeafPrivateKeys(s.Index, ss.InitPriv, ss.IdentityPriv)
}

func (s State) GetSecrets() StateSecrets {
	initPriv, _ := s.KeyPackage().PrivateKey()

	return StateSecrets{
		CipherSuite:      s.CipherSuite,
		Index:            s.Index,
		InitPriv:         initPriv,
		IdentityPriv:     s.IdentityPriv,
		Scheme:           s.Scheme,
		PendingProposals: s.PendingProposals,
		UpdateKeys:       s.UpdateKeys,
		Keys:             s.Keys,
		Tree:             s.Tree.GetSecrets(),
	}
}
